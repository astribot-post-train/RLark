package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
	"github.com/rlinf/rlark/apps/rlark/astribot/nodessh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
)

const nodeJoinNodeUID = "rlark.io/ssh-join-node-uid"

const (
	joinAttemptLogPrefix = "RLARK_SSH_JOIN "
	joinNodeUIDLogPrefix = "RLARK_JOIN_NODE_UID "
)

// The log is cleared before launching the unit. The attempt header makes
// recovery safe when an earlier unit's log survives a Gateway restart.
func joinInstallCommand(j *rlarkv1alpha1.NodeJoin, command string) string {
	return "export RLARK_SSH_JOIN_OWNER=" + nodessh.Quote(string(j.UID)) + "; exec >" + sshJoinLog + " 2>&1; printf '%s\\n' " +
		nodessh.Quote(joinAttemptLogPrefix+string(j.UID)) + "; (" + command + "); " +
		"echo \"" + sshJoinExit + "$?\""
}

func (g *Service) recordJoinedNodeUID(ctx context.Context, j *rlarkv1alpha1.NodeJoin, output string) error {
	if j.UID == "" {
		return nil
	}
	var uid string
	var currentAttempt bool
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if owner, ok := strings.CutPrefix(line, joinAttemptLogPrefix); ok {
			currentAttempt = owner == string(j.UID)
			continue
		}
		if !currentAttempt {
			continue
		}
		if found, ok := strings.CutPrefix(line, joinNodeUIDLogPrefix+j.Spec.NodeName+" "); ok {
			if len(validation.IsDNS1123Label(found)) != 0 || uid != "" && uid != found {
				return fmt.Errorf("conflicting or invalid native Node UID receipt")
			}
			uid = found
		}
	}
	if uid == "" {
		return nil
	}
	client := g.kubeClient.RlinfV1alpha1().NodeJoins()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := client.Get(ctx, j.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != j.UID {
			return fmt.Errorf("the SSH join was replaced")
		}
		if saved := current.Annotations[nodeJoinNodeUID]; saved != "" {
			if saved != uid {
				return fmt.Errorf("the native Node UID of this SSH join changed")
			}
			return nil
		}
		if err := g.verifyNativeJoinUID(ctx, current, uid); err != nil {
			return err
		}
		metav1.SetMetaDataAnnotation(&current.ObjectMeta, nodeJoinNodeUID, uid)
		_, err = client.Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
	if err == nil {
		metav1.SetMetaDataAnnotation(&j.ObjectMeta, nodeJoinNodeUID, uid)
	}
	return err
}

func (g *Service) verifyNativeJoinUID(ctx context.Context, j *rlarkv1alpha1.NodeJoin, uid string) error {
	if g.hooks.ProxyKubeRequest == nil {
		return fmt.Errorf("member API is unavailable to verify the SSH join's Node UID")
	}
	response, err := g.hooks.ProxyKubeRequest(ctx, http.MethodGet, provisionAgentID(j.Spec.ClusterID), "api/v1/nodes/"+j.Spec.NodeName, nil)
	if err != nil {
		return fmt.Errorf("verify the SSH join's Node UID: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	var node corev1.Node
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&node) != nil ||
		node.Name != j.Spec.NodeName || string(node.UID) != uid || node.DeletionTimestamp != nil {
		return fmt.Errorf("the member API did not confirm the SSH join's original Node UID")
	}
	return nil
}
