package singleton

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var kc client.Client

func GetClient() client.Client {
	return kc
}

func SetClient(cc client.Client) {
	kc = cc
}
