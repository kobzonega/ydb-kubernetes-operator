package tests

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/ydb-platform/ydb-kubernetes-operator/api/v1alpha1"
	testobjects "github.com/ydb-platform/ydb-kubernetes-operator/e2e/tests/test-objects"
)

const (
	Timeout  = time.Second * 600
	Interval = time.Second * 5
)

var HostPathDirectoryType corev1.HostPathType = "Directory"

func podIsReady(conditions []corev1.PodCondition) bool {
	for _, condition := range conditions {
		if condition.Type == testobjects.ReadyStatus && condition.Status == "True" {
			return true
		}
	}
	return false
}

func execInPod(namespace string, name string, cmd []string) (string, error) {
	args := []string{
		"-n",
		namespace,
		"exec",
		name,
		"--",
	}
	args = append(args, cmd...)
	result := exec.Command("kubectl", args...)
	stdout, err := result.Output()
	return string(stdout), err
}

func bringYdbCliToPod(namespace string, name string, ydbHome string) error {
	args := []string{
		"-n",
		namespace,
		"cp",
		fmt.Sprintf("%v/ydb/bin/ydb", os.ExpandEnv("$HOME")),
		fmt.Sprintf("%v:%v/ydb", name, ydbHome),
	}
	result := exec.Command("kubectl", args...)
	_, err := result.Output()
	return err
}

func installOperatorWithHelm(namespace string) bool {
	args := []string{
		"-n",
		namespace,
		"install",
		"ydb-operator",
		filepath.Join("..", "..", "deploy", "ydb-operator"),
		"-f",
		filepath.Join("..", "operator-values.yaml"),
	}
	result := exec.Command("helm", args...)
	stdout, err := result.Output()
	if err != nil {
		return false
	}

	return strings.Contains(string(stdout), "deployed")
}

func uninstallOperatorWithHelm(namespace string) bool {
	args := []string{
		"-n",
		namespace,
		"uninstall",
		"ydb-operator",
	}
	result := exec.Command("helm", args...)
	stdout, err := result.Output()
	if err != nil {
		return false
	}

	return strings.Contains(string(stdout), "uninstalled")
}

var _ = Describe("Operator smoke test", func() {
	var ctx context.Context
	var namespace corev1.Namespace

	var storageSample *v1alpha1.Storage
	var databaseSample *v1alpha1.Database

	BeforeEach(func() {
		storageSample = testobjects.DefaultStorage(filepath.Join(".", "data", "storage-block-4-2-config.yaml"))
		databaseSample = testobjects.DefaultDatabase()

		ctx = context.Background()
		namespace = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: testobjects.YdbNamespace,
			},
		}
		Expect(k8sClient.Create(ctx, &namespace)).Should(Succeed())
		Eventually(func(g Gomega) bool {
			namespaceList := corev1.NamespaceList{}
			g.Expect(k8sClient.List(ctx, &namespaceList)).Should(Succeed())
			for _, namespace := range namespaceList.Items {
				if namespace.GetName() == testobjects.YdbNamespace {
					return true
				}
			}
			return false
		}, Timeout, Interval).Should(BeTrue())
		Expect(installOperatorWithHelm(testobjects.YdbNamespace)).Should(BeTrue())
	})

	It("general smoke pipeline, create storage + database", func() {
		By("issuing create commands...")
		Expect(k8sClient.Create(ctx, storageSample)).Should(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, storageSample)).Should(Succeed())
		}()
		Expect(k8sClient.Create(ctx, databaseSample)).Should(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, databaseSample)).Should(Succeed())
		}()

		storage := v1alpha1.Storage{}
		Eventually(func(g Gomega) bool {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      storageSample.Name,
				Namespace: testobjects.YdbNamespace,
			}, &storage)).Should(Succeed())

			return meta.IsStatusConditionPresentAndEqual(
				storage.Status.Conditions,
				"StorageReady",
				metav1.ConditionTrue,
			) && storage.Status.State == testobjects.ReadyStatus
		}, Timeout, Interval).Should(BeTrue())

		By("checking that all the storage pods are running and ready...")
		storagePods := corev1.PodList{}
		Expect(k8sClient.List(ctx, &storagePods, client.InNamespace(testobjects.YdbNamespace), client.MatchingLabels{
			"ydb-cluster": "kind-storage",
		})).Should(Succeed())
		Expect(len(storagePods.Items)).Should(BeEquivalentTo(storageSample.Spec.Nodes))
		for _, pod := range storagePods.Items {
			Expect(pod.Status.Phase).To(BeEquivalentTo("Running"))
			Expect(podIsReady(pod.Status.Conditions)).To(BeTrue())
		}

		By("waiting until database is ready...")
		database := v1alpha1.Database{}
		Eventually(func(g Gomega) bool {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      databaseSample.Name,
				Namespace: testobjects.YdbNamespace,
			}, &database)).Should(Succeed())
			return meta.IsStatusConditionPresentAndEqual(
				database.Status.Conditions,
				"TenantInitialized",
				metav1.ConditionTrue,
			) && database.Status.State == testobjects.ReadyStatus
		}, Timeout, Interval).Should(BeTrue())

		By("checking that all the database pods are running and ready...")
		databasePods := corev1.PodList{}
		Expect(k8sClient.List(ctx, &databasePods, client.InNamespace(testobjects.YdbNamespace), client.MatchingLabels{
			"ydb-cluster": "kind-database",
		})).Should(Succeed())
		Expect(len(databasePods.Items)).Should(BeEquivalentTo(databaseSample.Spec.Nodes))
		for _, pod := range databasePods.Items {
			Expect(pod.Status.Phase).To(BeEquivalentTo("Running"))
			Expect(podIsReady(pod.Status.Conditions)).To(BeTrue())
		}

		firstDBPod := databasePods.Items[0].Name

		Expect(bringYdbCliToPod(testobjects.YdbNamespace, firstDBPod, testobjects.YdbHome)).To(Succeed())

		Eventually(func(g Gomega) {
			out, err := execInPod(testobjects.YdbNamespace, firstDBPod, []string{
				fmt.Sprintf("%v/ydb", testobjects.YdbHome),
				"-d",
				"/" + testobjects.DefaultDomain,
				"-e",
				"grpc://localhost:2135",
				"yql",
				"-s",
				"select 1",
			})

			g.Expect(err).To(BeNil())

			// `yql` gives output in the following format:
			// ┌─────────┐
			// | column0 |
			// ├─────────┤
			// | 1       |
			// └─────────┘
			g.Expect(strings.ReplaceAll(out, "\n", "")).
				To(MatchRegexp(".*column0.*1.*"))
		})
	})

	It("general smoke pipeline #2, create storage and database with nodeSets", func() {
		By("issuing create commands...")
		storageSample = testobjects.DefaultStorage(filepath.Join(".", "data", "storage-block-4-2-config-nodeSets.yaml"))
		databaseSample = testobjects.DefaultDatabase()
		testNodeSetName := "nodeset"
		for idx := 1; idx <= 2; idx++ {
			storageSample.Spec.NodeSet = append(storageSample.Spec.NodeSet, v1alpha1.StorageNodeSetSpecInline{
				Name:  testNodeSetName + "-" + strconv.Itoa(idx),
				Nodes: 4,
			})
			databaseSample.Spec.NodeSet = append(databaseSample.Spec.NodeSet, v1alpha1.DatabaseNodeSetSpecInline{
				Name:  testNodeSetName + "-" + strconv.Itoa(idx),
				Nodes: 4,
			})
		}
		Expect(k8sClient.Create(ctx, storageSample)).Should(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, storageSample)).Should(Succeed())
		}()
		Expect(k8sClient.Create(ctx, databaseSample)).Should(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, databaseSample)).Should(Succeed())
		}()

		storage := v1alpha1.Storage{}
		storageNodeSet1 := v1alpha1.StorageNodeSet{}
		storageNodeSet2 := v1alpha1.StorageNodeSet{}
		databaseNodeSet1 := v1alpha1.DatabaseNodeSet{}
		databaseNodeSet2 := v1alpha1.DatabaseNodeSet{}

		By("checking that storageNodeSets are ready...")
		Eventually(func(g Gomega) bool {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      storageSample.Name + "-nodeset-1",
				Namespace: testobjects.YdbNamespace,
			}, &storageNodeSet1)).Should(Succeed())

			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      storageSample.Name + "-nodeset-2",
				Namespace: testobjects.YdbNamespace,
			}, &storageNodeSet2)).Should(Succeed())

			return meta.IsStatusConditionPresentAndEqual(
				storageNodeSet1.Status.Conditions,
				"StorageNodeSetReady",
				metav1.ConditionTrue,
			) && storageNodeSet1.Status.State == testobjects.ReadyStatus &&
				meta.IsStatusConditionPresentAndEqual(
					storageNodeSet2.Status.Conditions,
					"StorageNodeSetReady",
					metav1.ConditionTrue,
				) && storageNodeSet2.Status.State == testobjects.ReadyStatus
		}, Timeout, Interval).Should(BeTrue())

		By("checking that storage are ready...")
		Eventually(func(g Gomega) bool {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      storageSample.Name,
				Namespace: testobjects.YdbNamespace,
			}, &storage)).Should(Succeed())

			return meta.IsStatusConditionPresentAndEqual(
				storage.Status.Conditions,
				"StorageReady",
				metav1.ConditionTrue,
			) && storage.Status.State == testobjects.ReadyStatus
		}, Timeout, Interval).Should(BeTrue())

		By("checking that all the storage pods are running and ready...")
		storagePods := corev1.PodList{}
		Expect(k8sClient.List(
			ctx, &storagePods, client.InNamespace(testobjects.YdbNamespace),
			client.MatchingLabels{"ydb-cluster": "kind-storage"}),
		).Should(Succeed())
		Expect(len(storagePods.Items)).Should(BeEquivalentTo(storageSample.Spec.Nodes))
		for _, pod := range storagePods.Items {
			Expect(pod.Status.Phase).To(BeEquivalentTo("Running"))
			Expect(podIsReady(pod.Status.Conditions)).To(BeTrue())
		}

		By("checking that databaseNodeSets are ready...")
		Eventually(func(g Gomega) bool {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      databaseSample.Name + "-nodeset-1",
				Namespace: testobjects.YdbNamespace,
			}, &databaseNodeSet1)).Should(Succeed())

			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      databaseSample.Name + "-nodeset-2",
				Namespace: testobjects.YdbNamespace,
			}, &databaseNodeSet2)).Should(Succeed())

			return meta.IsStatusConditionPresentAndEqual(
				databaseNodeSet1.Status.Conditions,
				"DatabaseNodeSetReady",
				metav1.ConditionTrue) &&
				databaseNodeSet1.Status.State == testobjects.ReadyStatus &&
				meta.IsStatusConditionPresentAndEqual(
					databaseNodeSet2.Status.Conditions,
					"DatabaseNodeSetReady",
					metav1.ConditionTrue) &&
				databaseNodeSet2.Status.State == testobjects.ReadyStatus
		}, Timeout, Interval).Should(BeTrue())

		By("waiting until database is ready...")
		database := v1alpha1.Database{}
		Eventually(func(g Gomega) bool {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      databaseSample.Name,
				Namespace: testobjects.YdbNamespace,
			}, &database)).Should(Succeed())
			return meta.IsStatusConditionPresentAndEqual(
				database.Status.Conditions,
				"TenantInitialized",
				metav1.ConditionTrue,
			) && database.Status.State == testobjects.ReadyStatus
		}, Timeout, Interval).Should(BeTrue())

		By("checking that all the database pods are running and ready...")
		databasePods := corev1.PodList{}
		Expect(k8sClient.List(ctx, &databasePods,
			client.InNamespace(testobjects.YdbNamespace),
			client.MatchingLabels{"ydb-cluster": "kind-database"},
		)).Should(Succeed())
		Expect(len(databasePods.Items)).Should(BeEquivalentTo(databaseSample.Spec.Nodes))
		for _, pod := range databasePods.Items {
			Expect(pod.Status.Phase).To(BeEquivalentTo("Running"))
			Expect(podIsReady(pod.Status.Conditions)).To(BeTrue())
		}

		By("delete nodeSetSpec inline...")
		databaseNodeSetList := v1alpha1.DatabaseNodeSetList{}
		database.Spec.Nodes = database.Spec.Nodes / 2
		database.Spec.NodeSet = database.Spec.NodeSet[1:]

		Eventually(func(g Gomega) bool {
			g.Expect(k8sClient.Update(ctx, &database)).Should(Succeed())
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      databaseSample.Name,
				Namespace: testobjects.YdbNamespace,
			}, &database)).Should(Succeed())
			g.Expect(k8sClient.List(ctx, &databaseNodeSetList,
				client.InNamespace(testobjects.YdbNamespace),
				client.MatchingLabels{"ydb-cluster": "kind-database"},
			)).Should(Succeed())
			return len(databaseNodeSetList.Items) == 1
		}, Timeout, Interval).Should(BeTrue())
		Expect(len(databaseNodeSetList.Items)).Should(BeEquivalentTo(len(database.Spec.NodeSet)))
		for _, databaseNodeSet := range databaseNodeSetList.Items {
			Expect(database.GetGeneration()).Should(BeEquivalentTo(databaseNodeSet.Status.ObservedDatabaseGeneration))
		}

		By("execute simple query inside ydb database pod...")
		firstDBPod := databasePods.Items[0].Name

		Expect(bringYdbCliToPod(testobjects.YdbNamespace, firstDBPod, testobjects.YdbHome)).To(Succeed())

		Eventually(func(g Gomega) {
			out, err := execInPod(testobjects.YdbNamespace, firstDBPod, []string{
				fmt.Sprintf("%v/ydb", testobjects.YdbHome),
				"-d",
				"/" + testobjects.DefaultDomain,
				"-e",
				"grpc://localhost:2135",
				"yql",
				"-s",
				"select 1",
			})

			g.Expect(err).To(BeNil())

			// `yql` gives output in the following format:
			// ┌─────────┐
			// | column0 |
			// ├─────────┤
			// | 1       |
			// └─────────┘
			g.Expect(strings.ReplaceAll(out, "\n", "")).
				To(MatchRegexp(".*column0.*1.*"))
		})
	})

	It("operatorConnection check, create storage with default staticCredentials", func() {
		By("issuing create commands...")
		storageSample = testobjects.DefaultStorage(filepath.Join(".", "data", "storage-block-4-2-config-staticCreds.yaml"))
		storageSample.Spec.OperatorConnection = &v1alpha1.ConnectionOptions{
			StaticCredentials: &v1alpha1.StaticCredentialsAuth{
				Username: "root",
			},
		}
		Expect(k8sClient.Create(ctx, storageSample)).Should(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, storageSample)).Should(Succeed())
		}()

		storage := v1alpha1.Storage{}
		Eventually(func(g Gomega) bool {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      storageSample.Name,
				Namespace: testobjects.YdbNamespace,
			}, &storage)).Should(Succeed())

			return meta.IsStatusConditionPresentAndEqual(
				storage.Status.Conditions,
				"StorageReady",
				metav1.ConditionTrue,
			) && storage.Status.State == testobjects.ReadyStatus
		}, Timeout, Interval).Should(BeTrue())

		By("checking that all the storage pods are running and ready...")
		storagePods := corev1.PodList{}
		Expect(k8sClient.List(ctx, &storagePods, client.InNamespace(testobjects.YdbNamespace), client.MatchingLabels{
			"ydb-cluster": "kind-storage",
		})).Should(Succeed())
		Expect(len(storagePods.Items)).Should(BeEquivalentTo(storageSample.Spec.Nodes))
		for _, pod := range storagePods.Items {
			Expect(pod.Status.Phase).To(BeEquivalentTo("Running"))
			Expect(podIsReady(pod.Status.Conditions)).To(BeTrue())
		}
	})

	It("storage.State goes Pending -> Preparing -> Provisioning -> Initializing -> Ready", func() {
		Expect(k8sClient.Create(ctx, storageSample)).Should(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, storageSample)).Should(Succeed())
		}()

		By("waiting until storage is ready...")
		storage := v1alpha1.Storage{}
		Eventually(func(g Gomega) bool {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      storageSample.Name,
				Namespace: testobjects.YdbNamespace,
			}, &storage)).Should(Succeed())

			return meta.IsStatusConditionPresentAndEqual(
				storage.Status.Conditions,
				"StorageReady",
				metav1.ConditionTrue,
			) && storage.Status.State == testobjects.ReadyStatus
		}, Timeout, Interval).Should(BeTrue())

		By("tracking storage state changes...")
		events, err := clientset.CoreV1().Events(testobjects.YdbNamespace).List(context.Background(),
			metav1.ListOptions{TypeMeta: metav1.TypeMeta{Kind: "Storage"}})
		Expect(err).ToNot(HaveOccurred())

		allowedChanges := map[string]string{
			"Pending":      "Preparing",
			"Preparing":    "Provisioning",
			"Provisioning": "Initializing",
			"Initializing": testobjects.ReadyStatus,
		}
		re := regexp.MustCompile(`Storage moved from ([a-zA-Z]+) to ([a-zA-Z]+)`)
		for _, event := range events.Items {
			if event.Reason == "StatusChanged" {
				match := re.FindStringSubmatch(event.Message)
				Expect(allowedChanges[match[1]]).To(BeEquivalentTo(match[2]))
			}
		}
	})

	AfterEach(func() {
		Expect(uninstallOperatorWithHelm(testobjects.YdbNamespace)).Should(BeTrue())
		Expect(k8sClient.Delete(ctx, &namespace)).Should(Succeed())
		Eventually(func(g Gomega) bool {
			namespaceList := corev1.NamespaceList{}
			g.Expect(k8sClient.List(ctx, &namespaceList)).Should(Succeed())
			for _, namespace := range namespaceList.Items {
				if namespace.GetName() == testobjects.YdbNamespace {
					return false
				}
			}
			return true
		}, Timeout, Interval).Should(BeTrue())
	})
})
