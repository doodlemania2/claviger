@description('Name of the Container Apps environment')
param environmentName string

@description('Name of the Container App')
param appName string

@description('Resource ID of the user-assigned managed identity. Created by modules/identity.bicep rather than here, so modules/keyvault.bicep can grant it Key Vault Secrets User before this app is written. A reference this identity cannot read may either fail this write or come up green with an empty env var — both are documented, see modules/identity.bicep. Observe the delivered value, never infer it from a green deploy.')
param identityId string

@description('Principal ID of that same user-assigned managed identity, used for the AcrPull role assignment below.')
param identityPrincipalId string

@description('Location for resources')
param location string

@description('Tags for resources')
param tags object = {}

@description('ACR login server')
param containerRegistryLoginServer string

@description('ACR name')
param containerRegistryName string

@description('Storage account name hosting the NFS pbdata file share')
param storageAccountName string

@description('Resource ID of the VNet subnet that the Container Apps environment runs in. Required for NFS Azure Files mounts.')
param subnetId string

// No secret VALUES are passed into this module. Every entry in the `secrets`
// array below is a Key Vault reference (keyVaultUrl + identity), which is what
// keeps `az containerapp secret show` from returning any of them in plaintext to
// a holder of Microsoft.App/containerApps/listSecrets/action. The values reach
// exactly one module, modules/keyvault.bicep.
//
// The "empty means omitted" contract is preserved: keyvault.bicep returns an
// empty URI for a secret it did not create, and each conditional below keys off
// that, so an environment without a given secret still provisions.

@description('Key Vault secret URI for the PocketBase admin email. Empty = omit PB_ADMIN_EMAIL.')
param pbAdminEmailSecretUri string = ''

@description('Key Vault secret URI for the PocketBase admin password. Empty = omit PB_ADMIN_PASSWORD.')
param pbAdminPasswordSecretUri string = ''

@description('Key Vault secret URI for the OTLP auth header. Empty = omit OTEL_EXPORTER_OTLP_HEADERS.')
param otlpAuthHeaderSecretUri string = ''


@description('Custom domain (e.g., auth.example.com). Leave empty to skip binding/managed cert.')
param customDomain string = ''

@description('Phase 2 flag: bind the managed certificate. Run first deploy with this false to add the hostname (which the cert provisioning requires), then re-run with true to issue + bind the cert.')
param bindCertificate bool = false

@description('Container image reference. Defaults to a placeholder; azd deploy replaces with the real image after build.')
param containerImage string = 'mcr.microsoft.com/k8se/quickstart:latest'

@description('WebAuthn relying-party ID (passkey effective domain, e.g. stfoafrisco.org). Decoupled from AppURL so passkeys can be scoped to a parent domain while auth/email links stay on a subdomain. Empty = the app falls back to the AppURL hostname at runtime.')
param webauthnRpId string = ''

@description('Comma-separated list of allowed WebAuthn origins (e.g. https://app.stfoafrisco.org). Empty = the app falls back to the AppURL origin at runtime.')
param webauthnRpOrigins string = ''

@description('OTLP collector ingest URL, e.g. https://otlp.thedoodleproject.net. Empty = telemetry export disabled entirely. Set the endpoint ROOT, not a signal path — the SDK appends /v1/logs itself.')
param otlpEndpoint string = ''

@description('Stable service.name for this app in SigNoz. The onboarding contract requires one per app and it must never change — it is the key SigNoz groups on. Empty resolves to the default below.')
param otelServiceName string = ''

@description('deployment.environment value. Use only: production | staging | development. Staging and production post to the SAME collector, so this is the only thing telling them apart. Empty is permitted and resolves to production — an unset GitHub Actions variable arrives as an empty string, not as unset, so rejecting empty here would fail the deployment outright.')
@allowed([
  ''
  'production'
  'staging'
  'development'
])
param otelEnvironment string = ''

@description('Minimum log level exported to the collector (DEBUG|INFO|WARN|ERROR). Empty exports everything, which for this app is ~8.6k health-probe records/day. Local SQLite logging is unaffected either way.')
param otelMinLevel string = ''

// Reference existing resources
resource acr 'Microsoft.ContainerRegistry/registries@2023-07-01' existing = {
  name: containerRegistryName
}

// RBAC: ACR Pull for container image access
var acrPullRoleId = '7f951dda-4ed3-4680-a7ca-43fe172d538d'

// Resolve the telemetry identity defaults in bicep rather than relying on azd's
// ${VAR=default} substitution: an unset GitHub Actions variable is passed to azd
// as an empty string, so the substitution default never fires and the resource
// attributes would ship blank — which lands this app in SigNoz's undifferentiated
// stream, exactly what the onboarding contract exists to prevent.
var resolvedOtelServiceName = empty(otelServiceName) ? 'stfoa-auth' : otelServiceName
var resolvedOtelEnvironment = empty(otelEnvironment) ? 'production' : otelEnvironment
// guid() still hashes the identity's resource ID, which the hoist into
// modules/identity.bicep did not change — so this assignment is not recreated.
resource acrRoleAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(acr.id, identityId, acrPullRoleId)
  scope: acr
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', acrPullRoleId)
    principalId: identityPrincipalId
    principalType: 'ServicePrincipal'
  }
}

// Container Apps Environment - VNet-integrated workload-profile env so NFS Azure Files mounts work.
// vnetConfiguration.infrastructureSubnetId cannot be changed after the env is created; recreating
// the env requires deleting the old one first (or using a new env name) and reissuing the managed cert.
resource environment 'Microsoft.App/managedEnvironments@2024-03-01' = {
  name: environmentName
  location: location
  tags: tags
  properties: {
    // Azure Log Analytics / App Insights are retired; telemetry goes to SigNoz over OTLP.
    // ARM rejects the string 'none'; a null destination is how "no logs" is expressed.
    appLogsConfiguration: {
      destination: null
    }
    workloadProfiles: [
      {
        name: 'Consumption'
        workloadProfileType: 'Consumption'
      }
    ]
    vnetConfiguration: {
      infrastructureSubnetId: subnetId
      internal: false
    }
  }
}

// /pb_data is mounted from a Premium FileStorage NFS share. NFS honors POSIX byte-range
// locks, which SQLite needs for WAL mode — SMB does not, hence the previous SQLITE_BUSY crash loop.
resource pbdataEnvStorage 'Microsoft.App/managedEnvironments/storages@2025-07-01' = {
  parent: environment
  name: 'pbdata'
  properties: {
    nfsAzureFile: {
      server: '${storageAccountName}.file.${az.environment().suffixes.storage}'
      shareName: '/${storageAccountName}/pbdata'
      accessMode: 'ReadWrite'
    }
  }
}

// Free Azure-managed TLS certificate for the custom domain (Phase 2 only).
// Container Apps requires the hostname to be present on a container app in the environment
// BEFORE the managed cert can be created, so this is gated behind `bindCertificate`.
// Phase 1 deploys the hostname with bindingType='Disabled' (no cert). Phase 2 sets
// bindCertificate=true to create the cert and switch the binding to SniEnabled.
resource managedCert 'Microsoft.App/managedEnvironments/managedCertificates@2024-03-01' = if (!empty(customDomain) && bindCertificate) {
  parent: environment
  name: empty(customDomain) ? 'placeholder' : replace(customDomain, '.', '-')
  location: location
  properties: {
    subjectName: customDomain
    domainControlValidation: 'CNAME'
  }
}

// Container App
resource containerApp 'Microsoft.App/containerApps@2024-03-01' = {
  name: appName
  location: location
  tags: union(tags, {
    'azd-service-name': 'pocketbase'
  })
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${identityId}': {}
    }
  }
  properties: {
    managedEnvironmentId: environment.id
    configuration: {
      activeRevisionsMode: 'Single'
      // Every entry is keyVaultUrl + identity, never `value`. A user-assigned
      // identity is mandatory here, not a preference: a system-assigned identity
      // does not exist until after the container app is created, so it cannot
      // resolve a reference present at create time.
      secrets: concat([], empty(pbAdminEmailSecretUri) ? [] : [
        { name: 'pb-admin-email', keyVaultUrl: pbAdminEmailSecretUri, identity: identityId }
      ], empty(pbAdminPasswordSecretUri) ? [] : [
        { name: 'pb-admin-password', keyVaultUrl: pbAdminPasswordSecretUri, identity: identityId }
      ], empty(otlpAuthHeaderSecretUri) ? [] : [
        { name: 'otlp-auth-header', keyVaultUrl: otlpAuthHeaderSecretUri, identity: identityId }
      ])
      ingress: {
        external: true
        targetPort: 8090
        transport: 'http'
        allowInsecure: false
        customDomains: empty(customDomain) ? [] : [
          bindCertificate ? {
            name: customDomain
            bindingType: 'SniEnabled'
            certificateId: managedCert.id
          } : {
            name: customDomain
            bindingType: 'Disabled'
          }
        ]
      }
      registries: [
        {
          server: containerRegistryLoginServer
          identity: identityId
        }
      ]
    }
    template: {
      containers: [
        {
          name: 'pocketbase'
          image: containerImage
          resources: {
            cpu: json('1.0')
            memory: '2Gi'
          }
          env: concat([
            { name: 'PB_HOST', value: '0.0.0.0' }
            { name: 'PB_PORT', value: '8090' }
          ], empty(pbAdminEmailSecretUri) ? [] : [
            { name: 'PB_ADMIN_EMAIL', secretRef: 'pb-admin-email' }
          ], empty(pbAdminPasswordSecretUri) ? [] : [
            { name: 'PB_ADMIN_PASSWORD', secretRef: 'pb-admin-password' }
          ], empty(webauthnRpId) ? [] : [
            { name: 'WEBAUTHN_RP_ID', value: webauthnRpId }
          ], empty(webauthnRpOrigins) ? [] : [
            { name: 'WEBAUTHN_RP_ORIGINS', value: webauthnRpOrigins }
          ], empty(otlpEndpoint) ? [] : [
            { name: 'OTEL_EXPORTER_OTLP_ENDPOINT', value: otlpEndpoint }
            // Not optional. The collector is reached through a Cloudflare Tunnel
            // that carries HTTP only; an SDK defaulting to gRPC on 4317 exports
            // nothing and reports no error — it simply never appears in SigNoz.
            { name: 'OTEL_EXPORTER_OTLP_PROTOCOL', value: 'http/protobuf' }
            // Both deployment.environment spellings on purpose: the SigNoz
            // environment filter reads the bare key, the current OpenTelemetry
            // semantic convention uses .name. Both cost nothing.
            {
              name: 'OTEL_RESOURCE_ATTRIBUTES'
              value: 'service.name=${resolvedOtelServiceName},service.namespace=stfoa,deployment.environment=${resolvedOtelEnvironment},deployment.environment.name=${resolvedOtelEnvironment}'
            }
          ], empty(otlpEndpoint) || empty(otlpAuthHeaderSecretUri) ? [] : [
            { name: 'OTEL_EXPORTER_OTLP_HEADERS', secretRef: 'otlp-auth-header' }
          ], empty(otelMinLevel) ? [] : [
            { name: 'PB_OTEL_MIN_LEVEL', value: otelMinLevel }
          ])
          probes: [
            {
              type: 'Startup'
              httpGet: {
                path: '/api/health'
                port: 8090
              }
              initialDelaySeconds: 5
              // 45 x 3s = 140s of grace. The entrypoint can spend up to
              // PB_HANDOVER_TIMEOUT (60s) waiting for the outgoing replica to
              // release /pb_data (#35) before PocketBase ever binds a port.
              periodSeconds: 3
              failureThreshold: 45
              timeoutSeconds: 3
            }
            {
              type: 'Liveness'
              httpGet: {
                path: '/api/health'
                port: 8090
              }
              initialDelaySeconds: 0
              periodSeconds: 30
              failureThreshold: 3
              timeoutSeconds: 5
            }
            {
              type: 'Readiness'
              httpGet: {
                path: '/api/health'
                port: 8090
              }
              initialDelaySeconds: 0
              periodSeconds: 10
              failureThreshold: 3
              timeoutSeconds: 5
            }
          ]
          volumeMounts: [
            {
              volumeName: 'pbdata'
              mountPath: '/pb_data'
            }
          ]
        }
      ]
      volumes: [
        {
          name: 'pbdata'
          storageType: 'NfsAzureFile'
          storageName: pbdataEnvStorage.name
        }
      ]
      scale: {
        minReplicas: 1
        maxReplicas: 1  // SQLite is single-writer — do not scale beyond 1
      }
      // 60s grace period gives PocketBase time to drain in-flight requests and
      // SQLite time to checkpoint the WAL on SIGTERM before the container is killed.
      terminationGracePeriodSeconds: 60
    }
  }
  dependsOn: [
    acrRoleAssignment
  ]
}

output fqdn string = containerApp.properties.configuration.ingress.fqdn
output customDomainVerificationId string = containerApp.properties.customDomainVerificationId
