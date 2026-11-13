package controller

import (
	"context"
	"fmt"
	"net"
	"net/url"

	stationv1alpha1 "github.com/rlinf/rlark/extensions/station/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
)

// keepMemberCredentials keeps the address in every member credential at what
// the SiteNetwork declares for the name the credential verifies the API by
// (tls-server-name). A control plane that moves to another machine is one
// changed line of spec.names; station-api and this controller read the Secret
// for every client they make, so both follow without being told.
//
// The address and not the name is what the credential holds because its
// readers run in the management machine's network, whose /etc/hosts a
// container copies once when it starts.
func (r *NetworkReconciler) keepMemberCredentials(ctx context.Context, spec stationv1alpha1.SiteNetworkSpec) error {
	address := map[string]string{}
	for _, name := range spec.Names {
		address[name.Name] = name.Address
	}
	for _, namespace := range r.MemberCredentialNamespaces {
		for _, site := range spec.Sites {
			for _, cluster := range site.Clusters {
				var secret corev1.Secret
				key := types.NamespacedName{Namespace: namespace, Name: MemberKubeconfigSecretName(cluster)}
				if err := r.reader.Get(ctx, key, &secret); apierrors.IsNotFound(err) {
					continue
				} else if err != nil {
					return fmt.Errorf("read the credential of member %s: %w", cluster, err)
				}
				config, err := clientcmd.Load(secret.Data[MemberKubeconfigSecretKey])
				if err != nil {
					return fmt.Errorf("the credential of member %s: %w", cluster, err)
				}
				moved := false
				for _, api := range config.Clusters {
					to, named := address[api.TLSServerName]
					server, err := url.Parse(api.Server)
					if !named || err != nil || server.Hostname() == to {
						continue
					}
					server.Host = net.JoinHostPort(to, server.Port())
					api.Server, moved = server.String(), true
				}
				if !moved {
					continue
				}
				if secret.Data[MemberKubeconfigSecretKey], err = clientcmd.Write(*config); err != nil {
					return fmt.Errorf("the credential of member %s: %w", cluster, err)
				}
				if err := r.client.Update(ctx, &secret); err != nil {
					return fmt.Errorf("move the credential of member %s: %w", cluster, err)
				}
			}
		}
	}
	return nil
}
