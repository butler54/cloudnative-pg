/*
This file is part of Cloud Native PostgreSQL.

Copyright (C) 2019-2021 EnterpriseDB Corporation.
*/

package e2e

import (
	"fmt"
	"strings"
	"time"

	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/specs"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/utils"
	"github.com/EnterpriseDB/cloud-native-postgresql/tests"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("PGDATA Corruption", func() {
	const (
		namespace   = "pg-data-corruption"
		sampleFile  = fixturesDir + "/pg_data_corruption/cluster-pg-data-corruption.yaml"
		clusterName = "cluster-pg-data-corruption"
	)
	JustAfterEach(func() {
		if CurrentGinkgoTestDescription().Failed {
			env.DumpClusterEnv(namespace, clusterName,
				"out/"+CurrentGinkgoTestDescription().TestText+".log")
		}
	})
	AfterEach(func() {
		err := env.DeleteNamespace(namespace)
		Expect(err).ToNot(HaveOccurred())
	})

	It("cluster can be recovered after pgdata corruption on primary", func() {
		var oldPrimaryPodName, oldPrimaryPVCName string
		var oldPrimaryPodInfo, newPrimaryPodInfo *corev1.Pod
		var err error
		tableName := "test_pg_data_corruption"
		err = env.CreateNamespace(namespace)
		Expect(err).ToNot(HaveOccurred())
		AssertCreateCluster(namespace, clusterName, sampleFile, env)
		// make sure jobs get deleted
		Eventually(func() (int, error) {
			jobList, err := env.GetJobList(namespace)
			if err != nil {
				return 1, err
			}
			return len(jobList.Items), err
		}, 60).Should(BeEquivalentTo(0))
		AssertCreateTestData(namespace, clusterName, tableName)
		By("gather current primary pod and pvc info", func() {
			oldPrimaryPodInfo, err = env.GetClusterPrimary(namespace, clusterName)
			Expect(err).ToNot(HaveOccurred())
			oldPrimaryPodName = oldPrimaryPodInfo.GetName()
			// Get the UID of the pod
			pvcName := oldPrimaryPodInfo.Spec.Volumes[0].PersistentVolumeClaim.ClaimName
			pvc := &corev1.PersistentVolumeClaim{}
			namespacedPVCName := types.NamespacedName{
				Namespace: namespace,
				Name:      pvcName,
			}
			err = env.Client.Get(env.Ctx, namespacedPVCName, pvc)
			Expect(err).ToNot(HaveOccurred())
			oldPrimaryPVCName = pvc.GetName()
		})
		By("corrupting primary pod by removing pg data", func() {
			cmd := fmt.Sprintf("kubectl exec %v -n %v postgres -- /bin/bash -c 'rm -fr %v/base/*'",
				oldPrimaryPodInfo.GetName(), namespace, specs.PgDataPath)
			_, _, err = tests.Run(cmd)
			Expect(err).ToNot(HaveOccurred())
		})
		By("verify failover after primary pod pg data corruption", func() {
			// check operator will perform a failover
			Eventually(func() string {
				newPrimaryPodInfo, err = env.GetClusterPrimary(namespace, clusterName)
				if err != nil {
					return ""
				}
				return newPrimaryPodInfo.GetName()
			}, 120, 5).ShouldNot(BeEquivalentTo(oldPrimaryPodName),
				"operator did not perform the failover")
		})
		By("verify the old primary pod health", func() {
			// old primary get restarted check that
			namespacedName := types.NamespacedName{
				Namespace: namespace,
				Name:      oldPrimaryPodName,
			}
			pod := &corev1.Pod{}
			err := env.Client.Get(env.Ctx, namespacedName, pod)
			Expect(err).ToNot(HaveOccurred())
			// The pod should be restarted and the count of the restarts should greater than 0
			Eventually(func() (int32, error) {
				pod := &corev1.Pod{}
				if err := env.Client.Get(env.Ctx, namespacedName, pod); err != nil {
					return 0, err
				}
				for _, data := range pod.Status.ContainerStatuses {
					if data.Name == specs.PostgresContainerName {
						return data.RestartCount, nil
					}
				}
				return int32(-1), nil
			}, 120).Should(BeNumerically(">", 0))
		})
		By("removing old primary pod and attached pvc", func() {
			// removing old primary pod attached pvc
			_, _, err = tests.Run(fmt.Sprintf("kubectl delete pvc  %v -n %v --wait=false", oldPrimaryPVCName, namespace))
			Expect(err).ToNot(HaveOccurred())

			zero := int64(0)
			forceDelete := &client.DeleteOptions{
				GracePeriodSeconds: &zero,
			}
			// Deleting old primary pod
			err = env.DeletePod(namespace, oldPrimaryPodName, forceDelete)
			Expect(err).ToNot(HaveOccurred())

			// checking that pod and pvc should be removed
			NamespacedName := types.NamespacedName{
				Namespace: namespace,
				Name:      oldPrimaryPodName,
			}
			Pod := &corev1.Pod{}
			err = env.Client.Get(env.Ctx, NamespacedName, Pod)
			Expect(err).To(HaveOccurred(), "pod %v is not deleted", oldPrimaryPodName)
		})
		By("verify new pod should join as standby", func() {
			newPodName := clusterName + "-4"
			newPodNamespacedName := types.NamespacedName{
				Namespace: namespace,
				Name:      newPodName,
			}
			Eventually(func() (bool, error) {
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, newPodNamespacedName, pod)
				if err != nil {
					return false, err
				}
				if utils.IsPodActive(*pod) || utils.IsPodReady(*pod) {
					return true, nil
				}
				return false, nil
			}, 300).Should(BeTrue())

			newPod := &corev1.Pod{}
			err = env.Client.Get(env.Ctx, newPodNamespacedName, newPod)
			Expect(err).ToNot(HaveOccurred())
			// check that pod should join as in recovery mode
			commandTimeout := time.Second * 5
			Eventually(func() (string, error) {
				stdOut, _, err := env.ExecCommand(env.Ctx, *newPod, specs.PostgresContainerName,
					&commandTimeout, "psql", "-U", "postgres", "app", "-tAc", "select pg_is_in_recovery();")
				return strings.Trim(stdOut, "\n"), err
			}, 60, 2).Should(BeEquivalentTo("t"))
			// verify test data
			AssertTestDataExistence(namespace, newPodName, tableName)
		})
		// verify test data on new primary
		newPrimaryPodInfo, err = env.GetClusterPrimary(namespace, clusterName)
		Expect(err).ToNot(HaveOccurred())
		AssertTestDataExistence(namespace, newPrimaryPodInfo.GetName(), tableName)
		assertClusterStandbysAreStreaming(namespace, clusterName)
	})
})