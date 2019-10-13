package cke

import (
	"k8s.io/client-go/tools/clientcmd/api"
)

// Kubeconfig creates *api.Config that will be rendered as "kubeconfig" file.
func Kubeconfig(cluster, user, ca, clientCrt, clientKey string) *api.Config {
	cfg := api.NewConfig()
	c := api.NewCluster()
	c.Server = "https://localhost:16443"
	c.CertificateAuthorityData = []byte(ca)
	cfg.Clusters[cluster] = c

	auth := api.NewAuthInfo()
	auth.ClientCertificateData = []byte(clientCrt)
	auth.ClientKeyData = []byte(clientKey)
	cfg.AuthInfos[user] = auth

	ctx := api.NewContext()
	ctx.AuthInfo = user
	ctx.Cluster = cluster
	cfg.Contexts["default"] = ctx
	cfg.CurrentContext = "default"

	return cfg
}

// UserKubeconfig makes kubeconfig for users
func UserKubeconfig(cluster, userName, ca, clientCrt, clientKey, server string) *api.Config {
	cfg := api.NewConfig()
	c := api.NewCluster()
	c.Server = server
	c.CertificateAuthorityData = []byte(ca)
	cfg.Clusters[cluster] = c

	auth := api.NewAuthInfo()
	auth.ClientCertificateData = []byte(clientCrt)
	auth.ClientKeyData = []byte(clientKey)
	cfg.AuthInfos[userName] = auth

	ctx := api.NewContext()
	ctx.AuthInfo = userName
	ctx.Cluster = cluster
	cfg.Contexts["default"] = ctx
	cfg.CurrentContext = "default"

	return cfg
}
