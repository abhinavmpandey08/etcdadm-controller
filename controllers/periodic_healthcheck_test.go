package controllers

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"testing"
	"time"

	etcdbootstrapv1 "github.com/aws/etcdadm-bootstrap-provider/api/v1beta1"
	etcdv1 "github.com/aws/etcdadm-controller/api/v1beta1"
	"github.com/go-logr/zapr"
	. "github.com/onsi/gomega"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/collections"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func TestStartHealthCheckLoopPaused(t *testing.T) {
	g := NewWithT(t)
	core, recordedLogs := observer.New(zapcore.InfoLevel)
	logger := zapr.NewLogger(zap.New(core))

	cluster := newClusterWithExternalEtcd()
	etcdadmCluster := newEtcdadmCluster(cluster, withPausedAnnotation)
	fakeClient := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(etcdadmCluster).Build()

	r := &EtcdadmClusterReconciler{
		Client:              fakeClient,
		Log:                 logger,
		HealthCheckInterval: time.Second, // override the healthcheck interval to 1 second
	}

	done := make(chan struct{})

	// Stop the healthcheck loop after 5 seconds
	go func() {
		time.Sleep(5 * time.Second)
		close(done)
	}()

	_ = &http.Client{
		Transport: RoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, nil
		}),
	}

	r.startHealthCheckLoop(context.Background(), done)

	g.Expect(recordedLogs.All()).To(Not(BeEmpty()))
	g.Expect(recordedLogs.All()[recordedLogs.Len()-1].Message).To(Equal("HealthCheck paused for EtcdadmCluster, skipping"))
}

type RoundTripperFunc func(*http.Request) (*http.Response, error)

func (fn RoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func TestStartHealthCheckLoop(t *testing.T) {
	g := NewWithT(t)
	core, recordedLogs := observer.New(zapcore.InfoLevel)
	logger := zapr.NewLogger(zap.New(core))

	cluster := newClusterWithExternalEtcd()
	etcdadmCluster := newEtcdadmCluster(cluster)
	etcdadmCluster.Status.CreationComplete = true
	fakeClient := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(etcdadmCluster).Build()

	// fakeClient.Create()
	r := &EtcdadmClusterReconciler{
		Client:              fakeClient,
		Log:                 logger,
		HealthCheckInterval: 1, // override the healthcheck interval to 1 second
	}
	r.SetIsPortOpen(func(_ context.Context, _ string) bool { return true })

	done := make(chan struct{})

	// Stop the healthcheck loop after 5 seconds
	go func() {
		time.Sleep(5 * time.Second)
		close(done)
	}()

	r.startHealthCheckLoop(context.Background(), done)

	g.Expect(recordedLogs.All()).To(Not(BeEmpty()))
	g.Expect(recordedLogs.All()[recordedLogs.Len()-1].Message).To(Equal("HealthCheck paused for EtcdadmCluster, skipping"))
}

// This test verifies that the periodicEtcdMembersHealthCheck does not panic when a Machine corresponding to an ETCD endpoint doesn not exist.
func TestReconcilePerodicHealthCheckEnsureNoPanic(t *testing.T) {
	cluster := newClusterWithExternalEtcd()
	etcdadmCluster := newEtcdadmCluster(cluster)
	ctx := context.Background()

	ownedMachine := newEtcdMachineWithEndpoint(etcdadmCluster, cluster)
	ownedMachines := make(collections.Machines, 1)
	ownedMachines.Insert(ownedMachine)

	etcdadmCluster.UID = "test-uid"
	etcdadmClusterMapper := make(map[types.UID]etcdadmClusterMemberHealthConfig, 1)

	ownedMachineEndpoint := ownedMachine.Status.Addresses[0].Address
	etcdadmCluster.Status.Endpoints = ownedMachineEndpoint

	// This translates to an ETCD endpoint that doesn't correspond to any machine.
	endpointToMachineMapper := make(map[string]*clusterv1.Machine)
	endpointToMachineMapper[ownedMachineEndpoint] = nil

	etcdadmClusterMapper[etcdadmCluster.UID] = etcdadmClusterMemberHealthConfig{
		unhealthyMembersFrequency: make(map[string]int),
		unhealthyMembersToRemove:  make(map[string]*clusterv1.Machine),
		endpointToMachineMapper:   endpointToMachineMapper,
		cluster:                   cluster,
		ownedMachines:             ownedMachines,
	}

	objects := []client.Object{
		cluster,
		etcdadmCluster,
		infraTemplate.DeepCopy(),
		ownedMachine,
	}
	fakeClient := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(objects...).Build()

	r := &EtcdadmClusterReconciler{
		Client:         fakeClient,
		uncachedClient: fakeClient,
		Log:            log.Log,
	}

	// This ensures that the test did not panic.
	defer func() {
		if rcv := recover(); rcv != nil {
			t.Errorf("code should not have panicked: %v", rcv)
		}
	}()

	_ = r.periodicEtcdMembersHealthCheck(ctx, cluster, etcdadmCluster, etcdadmClusterMapper)
}

// newEtcdMachineWithEndpoint returns a new machine with a random IP address.
func newEtcdMachineWithEndpoint(etcdadmCluster *etcdv1.EtcdadmCluster, cluster *clusterv1.Cluster) *clusterv1.Machine {
	machine := newEtcdMachine(etcdadmCluster, cluster)
	machine.Status.Addresses = []clusterv1.MachineAddress{
		{
			Type:    clusterv1.MachineExternalIP,
			Address: fmt.Sprintf("%d.%d.%d.%d", rand.Intn(256), rand.Intn(256), rand.Intn(256), rand.Intn(256)),
		},
	}
	return machine
}

type etcdadmClusterTest etcdv1.EtcdadmCluster

func getNewEtcdadmCluster(cluster *clusterv1.Cluster) *etcdadmClusterTest {
	etcdCluster := &etcdadmClusterTest{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      testEtcdadmClusterName,
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       "Cluster",
					APIVersion: clusterv1.GroupVersion.String(),
					Name:       cluster.Name,
					UID:        cluster.GetUID(),
				},
			},
			Finalizers: []string{etcdv1.EtcdadmClusterFinalizer},
		},
		Spec: etcdv1.EtcdadmClusterSpec{
			EtcdadmConfigSpec: etcdbootstrapv1.EtcdadmConfigSpec{
				CloudInitConfig: &etcdbootstrapv1.CloudInitConfig{
					Version: "v3.4.9",
				},
			},
			Replicas: pointer.Int32(int32(3)),
			InfrastructureTemplate: corev1.ObjectReference{
				Kind:       infraTemplate.GetKind(),
				APIVersion: infraTemplate.GetAPIVersion(),
				Name:       infraTemplate.GetName(),
				Namespace:  testNamespace,
			},
		},
	}
	return etcdCluster
}

// func (e *etcdadmClusterTest) withOwnerRef(cluster *clusterv1.Cluster) *etcdadmClusterTest{
// 	e.ObjectMeta.OwnerReferences = append(e.ObjectMeta.OwnerReferences, metav1.OwnerReference{
// 		APIVersion: ,
// 	})
// }
