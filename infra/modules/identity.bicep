@description('Name of the user-assigned managed identity')
param name string

@description('Location for the identity')
param location string

@description('Tags for resources')
param tags object = {}

// Hoisted out of container-app.bicep so the Key Vault module can grant this
// principal `Key Vault Secrets User` BEFORE the container app is created —
// impossible while the identity is declared inside the app's own module.
//
// Granting first is necessary but NOT sufficient: Key Vault data-plane RBAC can
// take up to 10 minutes to propagate after ARM reports the role assignment
// created (learn.microsoft.com/azure/container-apps/troubleshoot-deployment-errors
// #app-starts-with-missing-configuration).
//
// And an unresolvable reference has two documented outcomes, which is why the
// ordering is not the whole story. Microsoft says Container Apps validates that
// references resolve to a non-empty value at deployment time, rejecting the write
// (learn.microsoft.com/answers/a/12888502) — the safe case. But Learn's own
// troubleshooting flow also has a section for a reference that "is empty or
// missing at runtime", with `Authorization failed on Key Vault` listed as a cause.
// That case comes up GREEN with an empty env var, after acquire_single_writer has
// already drained the healthy outgoing replica.
//
// So observe the delivered value, never infer it from a green deploy — see
// DEPLOY.md, "Where the secrets live", for the post-deploy check.
//
// The resource name and resource group are unchanged by the hoist, so the
// identity is NOT recreated and the existing AcrPull role assignment GUID
// (derived from this identity's resource ID) stays stable.
resource identity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: name
  location: location
  tags: tags
}

output id string = identity.id
output name string = identity.name
output principalId string = identity.properties.principalId
output clientId string = identity.properties.clientId
