@description('Key Vault name (3-24 chars, globally unique, alphanumeric + dashes)')
@minLength(3)
@maxLength(24)
param name string

@description('Location for resources')
param location string

@description('Tags for resources')
param tags object = {}

@description('Principal ID of the managed identity that reads these secrets at runtime. Granted Key Vault Secrets User (read-only, data plane) on the whole vault.')
param readerPrincipalId string

@description('PocketBase admin email. Empty = the secret is not created and the container app omits it.')
@secure()
param pbAdminEmail string = ''

@description('PocketBase admin password. Empty = the secret is not created and the container app omits it.')
@secure()
param pbAdminPassword string = ''

@description('Full OTLP auth header, "Authorization=Bearer <token>". Empty = the secret is not created and telemetry export runs unauthenticated (i.e. disabled).')
@secure()
param otlpAuthHeader string = ''

// RBAC authorization (not access policies) is what makes the container app's
// managed identity the only runtime reader. It also closes the "Contributor can
// grant itself an access policy" path — though see DEPLOY.md: a Contributor can
// still flip this flag back, so this raises the bar rather than eliminating it.
//
// Purge protection is deliberately NOT enabled. It is irreversible, and
// `azd down --purge` has to be able to purge the soft-delete tombstone.
resource vault 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: name
  location: location
  tags: tags
  properties: {
    sku: {
      family: 'A'
      name: 'standard'
    }
    tenantId: subscription().tenantId
    enableRbacAuthorization: true
    enableSoftDelete: true
    softDeleteRetentionInDays: 7
    // Container Apps resolves secret references from the platform, not from the
    // app's VNet subnet, so a firewall/private endpoint here would break
    // resolution without extra plumbing. A private endpoint also costs ~$7/mo
    // per endpoint, against $0 for the public endpoint gated by RBAC.
    publicNetworkAccess: 'Enabled'
    networkAcls: {
      bypass: 'AzureServices'
      defaultAction: 'Allow'
    }
  }
}

// The deploying principal writes these through the ARM control plane
// (Microsoft.KeyVault/vaults/secrets/write, included in Contributor), NOT the
// data plane — so the CI service principal needs no Key Vault data-plane role.
// A human running `az keyvault secret set` DOES need Key Vault Secrets Officer.
resource pbAdminEmailSecret 'Microsoft.KeyVault/vaults/secrets@2023-07-01' = if (!empty(pbAdminEmail)) {
  parent: vault
  name: 'pb-admin-email'
  properties: {
    value: pbAdminEmail
  }
}

resource pbAdminPasswordSecret 'Microsoft.KeyVault/vaults/secrets@2023-07-01' = if (!empty(pbAdminPassword)) {
  parent: vault
  name: 'pb-admin-password'
  properties: {
    value: pbAdminPassword
  }
}

resource otlpAuthHeaderSecret 'Microsoft.KeyVault/vaults/secrets@2023-07-01' = if (!empty(otlpAuthHeader)) {
  parent: vault
  name: 'otlp-auth-header'
  properties: {
    value: otlpAuthHeader
  }
}

// Key Vault Secrets User — read secret contents only. No list, no write, no
// vault management. This is the whole of the container app's Key Vault access.
var keyVaultSecretsUserRoleId = '4633458b-17de-408a-b874-0445c86b69e6'

resource secretsUserRoleAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(vault.id, readerPrincipalId, keyVaultSecretsUserRoleId)
  scope: vault
  properties: {
    roleDefinitionId: subscriptionResourceId(
      'Microsoft.Authorization/roleDefinitions',
      keyVaultSecretsUserRoleId
    )
    principalId: readerPrincipalId
    principalType: 'ServicePrincipal'
  }
}

// All three secrets return VERSIONLESS URIs. Container Apps then picks up a new
// version within 30 minutes and restarts the active revision to apply it, so an
// out-of-band rotation lands without a deployment. At `maxReplicas: 1` that
// restart is an unscheduled downtime window — see DEPLOY.md, "Where the secrets
// live", before editing a secret in the portal.
//
// All three are safely rotatable in place, which is why versionless is right for
// all of them. A secret that is NOT rotatable in place must be version-pinned
// instead (`secretUriWithVersion`), so that one portal edit cannot auto-restart
// production into an unbootable state with no deployment and no review. The
// settings-encryption key (TDP-40) is exactly that case and must be pinned when
// it lands here.
//
// These are built as strings rather than read off the conditional secret
// resources on purpose — referencing a property of a resource whose `if ()` is
// false is an ARM evaluation hazard even inside a matching ternary.
// The outputs-should-not-contain-secrets suppressions below are correct, not a
// shortcut: the linter flags them only because the *condition* reads a secure
// parameter. What is emitted is a `https://<vault>/secrets/<name>` URI, which is
// public metadata — that is the entire point of a Key Vault reference.
output vaultName string = vault.name
output vaultUri string = vault.properties.vaultUri
#disable-next-line outputs-should-not-contain-secrets
output pbAdminEmailSecretUri string = empty(pbAdminEmail) ? '' : '${vault.properties.vaultUri}secrets/pb-admin-email'
#disable-next-line outputs-should-not-contain-secrets
output pbAdminPasswordSecretUri string = empty(pbAdminPassword) ? '' : '${vault.properties.vaultUri}secrets/pb-admin-password'
#disable-next-line outputs-should-not-contain-secrets
output otlpAuthHeaderSecretUri string = empty(otlpAuthHeader) ? '' : '${vault.properties.vaultUri}secrets/otlp-auth-header'
