package main

import (
	"time"

	ec2v1beta1 "github.com/crossplane-contrib/provider-aws/apis/ec2/v1beta1"
	pause "github.com/july2993/crossplane-pause"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func main() {
	var mgr manager.Manager
	// ...
	// setup mgr
	// ...

	r := pause.Reconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		GroupVersionKind:    ec2v1beta1.SubnetGroupVersionKind,
		UnPausePollInterval: pointer.Duration(time.Hour * 5),
		FrozenTimeDuration:  pointer.Duration(time.Minute * 5),
	}

	err := r.SetupWithManager(mgr)
	if err != nil {
		panic(err)
	}
}
