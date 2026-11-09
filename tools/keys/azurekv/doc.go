// Package azurekv wraps and unwraps data keys with an Azure Key Vault (or
// Managed HSM) key over the plain REST API. It has no dependency on the Azure
// SDK.
//
// Fork-local (Claviger #75, M4).
//
// The package is generic: it knows nothing about the field-encryption key
// hierarchy. tools/keys adapts [Client] to its KeySource interface.
//
// REST API version: 7.5 (GA data-plane version, the one used by the Microsoft
// Learn wrapkey/unwrapkey examples for Key Vault and Managed HSM).
//
// Authentication (no SDK): managed identity (IMDS and the App Service /
// Container Apps identity endpoint), workload identity federation and client
// secret. See [CredentialFromEnv].
//
// Nothing in this package logs, and no error text contains a token, a client
// secret, a federated assertion or key material.
package azurekv
