package callbacks

import (
	"fmt"

	"github.com/weibaohui/kom/kom"
)

// RegisterDenyMutationCallbacks registers the default callbacks and then denies
// mutation and exec actions before they reach the built-in handlers.
func RegisterDenyMutationCallbacks(c *kom.ClusterInst) func() {
	RegisterDefaultCallbacks(c)

	k := c.Kubectl
	_ = k.Callback().Delete().Before("kom:delete").Register("security:deny-delete", denyAction("delete"))
	_ = k.Callback().Exec().Before("kom:pod:exec").Register("security:deny-exec", denyAction("exec"))
	_ = k.Callback().Patch().Before("kom:patch").Register("security:deny-patch", denyAction("patch"))
	_ = k.Callback().Update().Before("kom:update").Register("security:deny-update", denyAction("update"))

	return nil
}

func denyAction(action string) func(*kom.Kubectl) error {
	return func(k *kom.Kubectl) error {
		stmt := k.Statement
		return fmt.Errorf(
			"operation denied by callback: %s is disabled (cluster=%s namespace=%s name=%s kind=%s)",
			action,
			k.ID,
			stmt.Namespace,
			stmt.Name,
			stmt.GVK.Kind,
		)
	}
}
