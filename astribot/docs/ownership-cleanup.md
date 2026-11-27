# UID-bound cleanup

Cluster provisioning cleanup records the original Cluster UID. Node removal for a failed SSH join uses the native Node UID from that attempt. The Gateway checks live resource identity and keeps Kubernetes delete preconditions. An earlier record without sufficient identity evidence may require separate ownership review.

In **Site Nodes**, a conflicting record with reason `NodeUIDChanged` has a **Clear stale enrollment** action. It removes the old enrollment by its own UID and resource version and preserves the current same-named Node.

A storage update or delete that encounters a conflicting version returns HTTP 409. Reload the declaration before retrying.
