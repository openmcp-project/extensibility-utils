# Extensibility Utils

A Go utility library for simplifying flux based OpenControlPlane service-providers with cross-cluster object management and Flux integration.

## Features

- **Object Manager**: Reconcile and manage Kubernetes objects across one or more clusters
- **Flux Integration**: Deploy Helm charts through Flux HelmRelease and OCIRepository resources
- **Secret Management**: Copy and manage Kubernetes secrets across clusters
- **Token Refresh**: Keep ServiceAccount tokens for MCP access fresh in workload cluster Secrets
- **Lifecycle Tracking**: Monitor object status and readiness with configurable policies
- **Dependency Management**: Define and respect object dependencies during reconciliation

## Package Documentation

### `pkg/objectmanager`

Core package for managing Kubernetes objects across clusters.

**Key Types:**
- `Manager`: Main interface for managing object lifecycle (Apply, Delete)
- `Cluster`: Represents a Kubernetes cluster containing objects
- `Object`: Wraps a client.Object with reconciliation and status functions
- `Cleaner`: Removes managed objects not in a specified list

**Key Features:**
- Dependency-aware reconciliation
- Configurable deletion policies (delete or orphan)
- Status tracking (Ready, Progressing, Terminating, Unknown)
- Multiple cleaner support for cleanup operations

### `pkg/flux`

Provides helpers for deploying Helm charts through Flux.

**Key Functions:**
- `ManageHelmRelease()`: Function to register a HelmRelease with a OCIRepository source

### `pkg/secret`

Utilities for managing Kubernetes secrets across clusters.

**Key Functions:**
- `ManagePullSecret()`: Register an image-pull secret copy
- `NewCleaner()`: Create a cleaner for managed pull secrets
- `PrefixName()`: Prefix and validate secret names to Kubernetes limits

### `pkg/tokenrefresh`

Controller that keeps a ServiceAccount token from a ManagedControlPlane in a Secret on a workload cluster,
independent of the service provider's main reconcile loop. The Secret is created by the caller; the
controller only writes the `token`, `ca.crt` and `namespace` keys and the
`<service-provider>.services.open-control-plane.io/token-creation-time` and `.../token-expiration-time`
annotations. Tokens are refreshed at 80% of their actual validity (the API server may grant a shorter
token than requested). A Secret that has a token but no creation annotation (e.g. written by an older
provider version) is refreshed on the first reconcile.

The caller should mint the first token with `EnsureToken()` while creating the Secret and leave an
existing token untouched afterwards, so a pod mounting the Secret as a required volume never sees it empty.

**Key Types:**
- `Reconciler[T]`: Generic controller for the service provider API type `T`
- `Binding`: ServiceAccount on the MCP cluster and Secret on the workload cluster
- `ClusterResolver`: Read-only function returning the MCP and workload cluster of an object

**Key Functions:**
- `SetupWithManager()`: Register the controller with a manager
- `EnsureToken()`: Mint a token and write it plus annotations into a Secret in memory (for the initial token)
- `ExpirationAnnotation()` / `CreationAnnotation()`: Annotation keys the controller writes on the Secret

```go
r := &tokenrefresh.Reconciler[*v1alpha1.Velero]{
	Client:              mgr.GetClient(),
	ServiceProviderName: "velero",
	NewObject:           func() *v1alpha1.Velero { return &v1alpha1.Velero{} },
	Clusters:            resolveClusters, // ClusterResolver returning the MCP and workload *clusters.Cluster
	Binding:             tokenBinding,
}
err := r.SetupWithManager(mgr, "velero-token-refresh")
```

## Support, Feedback, Contributing

This project is open to feature requests/suggestions, bug reports etc. via [GitHub issues](https://github.com/openmcp-project/repository-template/issues). Contribution and feedback are encouraged and always welcome. For more information about how to contribute, the project structure, as well as additional contribution information, see our [Contribution Guidelines](https://github.com/openmcp-project/.github/blob/main/CONTRIBUTING.md).

## Security / Disclosure

If you find any bug that may be a security problem, please follow our instructions at [in our security policy](https://github.com/openmcp-project/repository-template/security/policy) on how to report it. Please do not create GitHub issues for security-related doubts or problems.

## Code of Conduct

We as members, contributors, and leaders pledge to make participation in our community a harassment-free experience for everyone. By participating in this project, you agree to abide by its [Code of Conduct](https://github.com/openmcp-project/.github/blob/main/CODE_OF_CONDUCT.md) at all times.

## Licensing

Copyright OpenControlPlane contributors. Please see our [LICENSE](LICENSE) for copyright and license information. Detailed information including third-party components and their licensing/copyright information is available [via the REUSE tool](https://api.reuse.software/info/github.com/openmcp-project/repository-template).

---

<p align="center">
  <a href="https://apeirora.eu/content/projects/">
    <img alt="BMWK-EU funding logo" src="https://apeirora.eu/assets/img/BMWK-EU.png" width="300"/>
  </a>
</p>

<p align="center">
  OpenControlPlane is part of <a href="https://apeirora.eu/content/projects/">ApeiroRA</a>, an EU Important Project of Common European Interest (IPCEI-CIS).
</p>

<p align="center">
  Copyright Linux Foundation Europe. For web site terms of use, trademark policy and other project policies please see <a href="https://linuxfoundation.eu/en/policies">https://linuxfoundation.eu/en/policies</a>.
</p>
