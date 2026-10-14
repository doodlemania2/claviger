targetScope = 'subscription'

@minLength(1)
@maxLength(64)
@description('Name of the environment (e.g., dev, staging, prod)')
param environmentName string

@minLength(1)
@description('Primary location for all resources')
param location string = 'westus'

@description('PocketBase admin email for initial superuser')
@secure()
param pbAdminEmail string = ''

@description('PocketBase admin password for initial superuser')
@secure()
param pbAdminPassword string = ''

@description('Resource group name for this deployment. Defaults to rg-<environmentName>.')
param resourceGroupName string = 'rg-${environmentName}'

@description('Storage account name (3-24 chars, lowercase alphanumeric only). Defaults to a deterministic name derived from the resource token.')
@minLength(3)
@maxLength(24)
param storageAccountName string = 'st${uniqueString(subscription().subscriptionId, environmentName, location)}'

@description('Custom domain to bind to the Container App ingress (leave empty on first deploy to obtain the verification ID, then add DNS records and redeploy with this set). Example: auth.example.com')
param customDomain string = ''

@description('Phase 2 flag for managed cert. Deploy once with this false to add the hostname, then re-run with true to issue the cert and switch to SniEnabled.')
param bindCertificate bool = false

@description('Container image reference. Leave default; azd deploy replaces it with the freshly built image after the first provision.')
param containerImage string = 'mcr.microsoft.com/k8se/quickstart:latest'

@description('WebAuthn relying-party ID (passkey effective domain, e.g. stfoafrisco.org). Decoupled from AppURL so passkeys can be scoped to a parent domain. Empty = fall back to the AppURL hostname at runtime.')
param webauthnRpId string = ''

@description('Comma-separated allowed WebAuthn origins (e.g. https://app.stfoafrisco.org). Empty = fall back to the AppURL origin at runtime.')
param webauthnRpOrigins string = ''

@description('OTLP collector ingest URL (endpoint root, not a signal path). Empty = telemetry export disabled.')
param otlpEndpoint string = ''

@description('Full OTLP auth header, "Authorization=Bearer <token>". Supply via Key Vault reference; never commit the token.')
@secure()
param otlpAuthHeader string = ''

@description('Stable service.name for this app in SigNoz. Never change it once set.')
param otelServiceName string = ''

@description('deployment.environment value: production | staging | development.')
param otelEnvironment string = ''

@description('Minimum log level exported to the collector (DEBUG|INFO|WARN|ERROR). Empty exports everything.')
param otelMinLevel string = ''

var abbrs = {
  containerAppsEnvironment: 'cae'
  containerApp: 'ca'
  containerRegistry: 'cr'
  managedIdentity: 'id'
  virtualNetwork: 'vnet'
  subnet: 'snet-aca'
}

var resourceToken = uniqueString(subscription().subscriptionId, environmentName, location)
var tags = {
  'azd-env-name': environmentName
}

resource rg 'Microsoft.Resources/resourceGroups@2024-03-01' = {
  name: resourceGroupName
  location: location
  tags: tags
}

module acr 'modules/acr.bicep' = {
  name: 'acr'
  scope: rg
  params: {
    name: '${abbrs.containerRegistry}${resourceToken}'
    location: location
    tags: tags
  }
}

module network 'modules/network.bicep' = {
  name: 'network'
  scope: rg
  params: {
    vnetName: '${abbrs.virtualNetwork}-${environmentName}'
    subnetName: abbrs.subnet
    location: location
  }
}

module storage 'modules/storage.bicep' = {
  name: 'storage'
  scope: rg
  params: {
    name: storageAccountName
    location: location
    tags: tags
    subnetId: network.outputs.subnetId
  }
}

module containerApp 'modules/container-app.bicep' = {
  name: 'container-app'
  scope: rg
  params: {
    environmentName: '${abbrs.containerAppsEnvironment}-${environmentName}'
    appName: '${abbrs.containerApp}-${environmentName}'
    identityName: '${abbrs.managedIdentity}-${environmentName}'
    location: location
    tags: tags
    containerRegistryLoginServer: acr.outputs.loginServer
    containerRegistryName: acr.outputs.name
    storageAccountName: storage.outputs.storageAccountName
    subnetId: network.outputs.subnetId
    pbAdminEmail: pbAdminEmail
    pbAdminPassword: pbAdminPassword
    customDomain: customDomain
    bindCertificate: bindCertificate
    containerImage: containerImage
    webauthnRpId: webauthnRpId
    webauthnRpOrigins: webauthnRpOrigins
    otlpEndpoint: otlpEndpoint
    otlpAuthHeader: otlpAuthHeader
    otelServiceName: otelServiceName
    otelEnvironment: otelEnvironment
    otelMinLevel: otelMinLevel
  }
}

output AZURE_CONTAINER_REGISTRY_ENDPOINT string = acr.outputs.loginServer
output AZURE_CONTAINER_REGISTRY_NAME string = acr.outputs.name
output AZURE_CONTAINER_APP_FQDN string = containerApp.outputs.fqdn
output AZURE_CONTAINER_APP_CUSTOM_DOMAIN_VERIFICATION_ID string = containerApp.outputs.customDomainVerificationId
output AZURE_RESOURCE_GROUP string = rg.name
