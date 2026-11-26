package provisioning

// ClusterTypeLabel records the site classification of a Cluster.
const ClusterTypeLabel = "rlark.io/cluster-type"

func ValidClusterType(value string) bool { return value == "gpu" || value == "robot" }
