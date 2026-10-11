// Package v1alpha1 holds the API schema: resource types and gRPC services.
// The .proto files are the single source of truth (ADR 0001); the Go code is
// generated with `make proto` and checked in.
package v1alpha1

// API version and kinds.
const (
	APIVersion = "v1alpha1"

	KindWorkload    = "Workload"
	KindNode        = "Node"
	KindReplica     = "Replica"
	KindSecret      = "Secret"
	KindConfig      = "Config"
	KindService     = "Service"
	KindRoute       = "Route"
	KindVolume      = "Volume"
	KindVolumeClaim = "VolumeClaim" // internal: one per Volume in use, see ADR 0016
	KindJoinToken   = "JoinToken"   // internal
	KindUIAuth      = "UIAuth"      // internal

	// Internal kinds for automatic TLS (ADR 0015, pass C). Not user-appliable.
	KindCertificate   = "Certificate"
	KindAcmeAccount   = "AcmeAccount"
	KindAcmeChallenge = "AcmeChallenge"
	// KindRouteClaim reserves a Route's host and path prefix (ADR 0015, pass
	// E). Its name is the reserved key; its spec is the owning Route's name.
	KindRouteClaim = "RouteClaim"

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
