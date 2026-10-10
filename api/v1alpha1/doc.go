// Package v1alpha1 holds the API schema: resource types and gRPC services.
// The .proto files are the single source of truth (ADR 0001); the Go code is
// generated with `make proto` and checked in.
package v1alpha1

// API version and kinds.
const (
	APIVersion = "v1alpha1"

	KindWorkload  = "Workload"
	KindNode      = "Node"
	KindReplica   = "Replica"
	KindSecret    = "Secret"
	KindConfig    = "Config"
	KindService   = "Service"
	KindRoute     = "Route"
	KindJoinToken = "JoinToken" // internal
	KindUIAuth    = "UIAuth"    // internal

	// DNSDomain is the domain of Service names: <service>.shoal.
	DNSDomain = "shoal"
)

// Node phases.
const (
	NodeReady    = "Ready"
	NodeNotReady = "NotReady"
)

// Replica phases.
const (
	ReplicaPending   = "Pending"
	ReplicaRunning   = "Running"
	ReplicaSucceeded = "Succeeded"
	ReplicaFailed    = "Failed"
	ReplicaStopped   = "Stopped"
)
