package crossplane

import (
	"context"
	"testing"
	"time"

	ec2v1beta1 "github.com/crossplane-contrib/provider-aws/apis/ec2/v1beta1"
	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPause(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ec2v1beta1.SchemeBuilder.AddToScheme(scheme)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	subnetTPL := &ec2v1beta1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-subnet",
			Annotations: map[string]string{
				"some": "value",
			},
		},
		Spec: ec2v1beta1.SubnetSpec{
			ForProvider: ec2v1beta1.SubnetParameters{
				CIDRBlock: "a",
			},
		},
		Status: ec2v1beta1.SubnetStatus{
			AtProvider: ec2v1beta1.SubnetObservation{
				SubnetID: "a",
			},
		},
	}

	var err error
	var subnet *ec2v1beta1.Subnet
	var u *unstructured.Unstructured

	setValue := func(t *testing.T) {
		t.Helper()
		var err error

		u = &unstructured.Unstructured{}
		u.Object, err = runtime.DefaultUnstructuredConverter.ToUnstructured(subnet)
		u.SetGroupVersionKind(ec2v1beta1.SubnetGroupVersionKind)
		require.Nil(t, err)
	}

	tmpDuration := time.Hour
	unPauseInterval := &tmpDuration

	// create the object and pause it.
	subnet = subnetTPL.DeepCopy()
	err = cli.Create(ctx, subnet)
	require.Nil(t, err)
	setValue(t)
	err = ensurePause(ctx, cli, u, nil, unPauseInterval, "test")
	require.Nil(t, err)
	// read back and check
	u = &unstructured.Unstructured{}
	u.SetGroupVersionKind(ec2v1beta1.SubnetGroupVersionKind)
	err = cli.Get(ctx, client.ObjectKeyFromObject(subnetTPL), u)
	require.Nil(t, err)
	info, err := parsePauseInfo(u)
	require.Nil(t, err)
	require.True(t, info.Pause)
	require.NotNil(t, info.LastPauseTime)
	require.NotNil(t, info.ShouldUnpauseTime)
	rate := float64(info.ShouldUnpauseTime.Sub(info.LastPauseTime.Time)) / float64(*unPauseInterval)
	require.True(t, rate >= 1.0 && rate <= 1.1)
	require.Equal(t, "true", u.GetAnnotations()[AnnotationKeyReconciliationPaused])
	// unpause it
	err = ensureUnPause(ctx, cli, u, info, "test")
	require.Nil(t, err)
	// read back and check
	u = &unstructured.Unstructured{}
	u.SetGroupVersionKind(ec2v1beta1.SubnetGroupVersionKind)
	err = cli.Get(ctx, client.ObjectKeyFromObject(subnetTPL), u)
	require.Nil(t, err)
	info, err = parsePauseInfo(u)
	require.Nil(t, err)
	require.False(t, info.Pause)
	require.Nil(t, info.Object)
	require.NotNil(t, info.LastUnPauseTime)
	require.Equal(t, "", u.GetAnnotations()[AnnotationKeyReconciliationPaused])
}

func TestIsUpdated(t *testing.T) {
	ctx := context.Background()

	subnet := ec2v1beta1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-subnet",
			Annotations: map[string]string{
				"some": "value",
			},
		},
		Spec: ec2v1beta1.SubnetSpec{
			ForProvider: ec2v1beta1.SubnetParameters{
				CIDRBlock: "a",
			},
		},
		Status: ec2v1beta1.SubnetStatus{
			AtProvider: ec2v1beta1.SubnetObservation{
				SubnetID: "a",
			},
		},
	}

	var oldSubnet, nowSubnet *ec2v1beta1.Subnet
	var old, now *unstructured.Unstructured

	setValue := func(t *testing.T) {
		t.Helper()
		var err error

		old = &unstructured.Unstructured{}
		old.Object, err = runtime.DefaultUnstructuredConverter.ToUnstructured(oldSubnet)
		require.Nil(t, err)

		now = &unstructured.Unstructured{}
		now.Object, err = runtime.DefaultUnstructuredConverter.ToUnstructured(nowSubnet)
		require.Nil(t, err)
	}

	// test equal
	oldSubnet = subnet.DeepCopy()
	nowSubnet = subnet.DeepCopy()
	nowSubnet.Annotations[AnnotationKeyPauseInfo] = "value"
	nowSubnet.Annotations[AnnotationKeyReconciliationPaused] = "true"
	setValue(t)
	updated, err := isUpdated(ctx, old, now)
	require.Nil(t, err)
	require.False(t, updated)

	// test spec updated
	oldSubnet = subnet.DeepCopy()
	nowSubnet = subnet.DeepCopy()
	nowSubnet.Spec.ForProvider.CIDRBlock = "b"
	setValue(t)
	updated, err = isUpdated(ctx, old, now)
	require.Nil(t, err)
	require.True(t, updated)

	// test label updated
	oldSubnet = subnet.DeepCopy()
	nowSubnet = subnet.DeepCopy()
	nowSubnet.Labels = map[string]string{"a": "b"}
	setValue(t)
	updated, err = isUpdated(ctx, old, now)
	require.Nil(t, err)
	require.True(t, updated)

	// test ann updated
	oldSubnet = subnet.DeepCopy()
	nowSubnet = subnet.DeepCopy()
	nowSubnet.Annotations = map[string]string{"a": "b"}
	setValue(t)
	updated, err = isUpdated(ctx, old, now)
	require.Nil(t, err)
	require.True(t, updated)
}

func TestGetCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ec2v1beta1.SchemeBuilder.AddToScheme(scheme)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	subnet := ec2v1beta1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-subnet",
		},
		Spec: ec2v1beta1.SubnetSpec{},
	}

	available := xpv1.Available()
	// After Marshal available.LastTransitionTime will lost the nanosec part so we overwrite it.
	tm, err := time.Parse(time.RFC3339, "2022-07-22T10:54:18Z")
	tm = tm.Local()
	require.Nil(t, err)
	available.LastTransitionTime = metav1.NewTime(tm)
	subnet.SetConditions(available)

	success := xpv1.ReconcileSuccess()
	success.LastTransitionTime = metav1.NewTime(tm)
	subnet.SetConditions(success)

	err = cli.Create(ctx, &subnet)
	require.Nil(t, err)

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ec2v1beta1.SubnetGroupVersionKind)
	err = cli.Get(ctx, client.ObjectKeyFromObject(&subnet), u)
	require.Nil(t, err)

	res, err := getCondition(u, xpv1.TypeReady)
	require.Nil(t, err)
	require.Equal(t, available, *res)

	res, err = getCondition(u, xpv1.TypeSynced)
	require.Nil(t, err)
	require.Equal(t, success, *res)

	res, err = getCondition(u, xpv1.ConditionType("not-exist"))
	require.Nil(t, err)
	require.Nil(t, res)
	return
}

func TestGVK(t *testing.T) {
	gvk := ec2v1beta1.SubnetGroupVersionKind
	t.Log(gvk)
	// output:
	// ec2.aws.crossplane.io/v1beta1, Kind=Subnet
}
