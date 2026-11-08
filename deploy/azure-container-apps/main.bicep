// Claviger on Azure Container Apps, with SQLite on an Azure Files NFS share.
// Generic template: every name derives from `name`. See README.md.

targetScope = 'resourceGroup'

@description('Base name for all resources. 3 to 20 lowercase letters or digits.')
@minLength(3)
@maxLength(20)
param name string

param location string = resourceGroup().location

@description('Release tag of ghcr.io/doodlemania2/claviger. Pin a version, do not use latest.')
param clavigerVersion string

@description('Exactly 32 characters. Encrypts the settings stored in SQLite. Keep a copy.')
@secure()
@minLength(32)
@maxLength(32)
param encryptionKey string

@description('Email of the first superuser. Leave empty to skip the bootstrap.')
param adminEmail string = ''

@description('Password of the first superuser. Only used on first start.')
@secure()
param adminPassword string = ''

@description('Optional. Turns on GET /api/metrics for Prometheus when set.')
@secure()
param metricsToken string = ''

@description('Optional. OTLP/HTTP endpoint for log export. Empty = off.')
param otlpEndpoint string = ''

@description('Days to keep audit events. 0 = keep forever.')
param auditRetentionDays int = 0

@description('Share size in GiB. Premium file shares start at 100.')
@minValue(100)
param shareSizeGiB int = 100

@description('Seconds an incoming replica waits for the outgoing one to release /pb_data. 0 disables the handover (unsafe).')
param handoverTimeoutSeconds int = 60

@description('Seconds between handover checks on the outgoing replica.')
param handoverPollSeconds int = 2

param cpu string = '0.5'
param memory string = '1Gi'

var suffix = uniqueString(resourceGroup().id, name)
// Storage account names: 3-24 lowercase letters and digits, globally unique.
var storageName = take('${replace(toLower(name), '-', '')}${suffix}', 24)
var shareName = 'pbdata'
var image = 'ghcr.io/doodlemania2/claviger:${clavigerVersion}'

resource vnet 'Microsoft.Network/virtualNetworks@2023-11-01' = {
  name: '${name}-vnet'
  location: location
  properties: {
    addressSpace: { addressPrefixes: ['10.40.0.0/16'] }
    subnets: [
      {
        name: 'aca'
        properties: {
          addressPrefix: '10.40.0.0/23'
          // NFS from a container app needs the storage service endpoint on the
          // environment subnet, and the subnet delegated to Container Apps.
          serviceEndpoints: [{ service: 'Microsoft.Storage' }]
          delegations: [
            {
              name: 'aca'
              properties: { serviceName: 'Microsoft.App/environments' }
            }
          ]
        }
      }
    ]
  }
}

// Azure Files NFS exists only on premium (FileStorage) accounts.
resource storage 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: storageName
  location: location
  kind: 'FileStorage'
  sku: { name: 'Premium_LRS' }
  properties: {
    // NFS shares do not support HTTPS-only transport.
    supportsHttpsTrafficOnly: false
    minimumTlsVersion: 'TLS1_2'
    allowBlobPublicAccess: false
    // NFS has no user authentication. Access is controlled by the network:
    // only the Container Apps subnet may reach the account.
    publicNetworkAccess: 'Enabled'
    networkAcls: {
      defaultAction: 'Deny'
      bypass: 'None'
      virtualNetworkRules: [
        {
          id: vnet.properties.subnets[0].id
          action: 'Allow'
        }
      ]
    }
  }
}

resource fileService 'Microsoft.Storage/storageAccounts/fileServices@2023-05-01' = {
  parent: storage
  name: 'default'
}

resource share 'Microsoft.Storage/storageAccounts/fileServices/shares@2023-05-01' = {
  parent: fileService
  name: shareName
  properties: {
    enabledProtocols: 'NFS'
    shareQuota: shareSizeGiB
    // The image runs as root. Root squash would remap it to nobody and writes would fail.
    rootSquash: 'NoRootSquash'
  }
}

resource logs 'Microsoft.OperationalInsights/workspaces@2023-09-01' = {
  name: '${name}-logs'
  location: location
  properties: {
    sku: { name: 'PerGB2018' }
    retentionInDays: 30
  }
}

resource env 'Microsoft.App/managedEnvironments@2025-01-01' = {
  name: '${name}-env'
  location: location
  properties: {
    // NFS mounts need a VNet-integrated, workload-profiles environment.
    vnetConfiguration: {
      infrastructureSubnetId: vnet.properties.subnets[0].id
    }
    workloadProfiles: [
      { name: 'Consumption', workloadProfileType: 'Consumption' }
    ]
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: logs.properties.customerId
        sharedKey: logs.listKeys().primarySharedKey
      }
    }
  }
}

resource envStorage 'Microsoft.App/managedEnvironments/storages@2025-01-01' = {
  parent: env
  name: 'pbdata'
  properties: {
    nfsAzureFile: {
      server: '${storage.name}.file.${environment().suffixes.storage}'
      // NFS shares are exported as /<account>/<share>.
      shareName: '/${storage.name}/${shareName}'
      accessMode: 'ReadWrite'
    }
  }
  dependsOn: [share]
}

var baseEnv = [
  { name: 'PB_HOST', value: '0.0.0.0' }
  { name: 'PB_PORT', value: '8090' }
  { name: 'PB_ENCRYPTION_KEY', secretRef: 'encryption-key' }
  // Single-writer handover. See README.md.
  { name: 'PB_HANDOVER_TIMEOUT', value: string(handoverTimeoutSeconds) }
  { name: 'PB_HANDOVER_POLL', value: string(handoverPollSeconds) }
]
var adminEnv = empty(adminEmail) || empty(adminPassword) ? [] : [
  { name: 'PB_ADMIN_EMAIL', value: adminEmail }
  { name: 'PB_ADMIN_PASSWORD', secretRef: 'admin-password' }
]
var metricsEnv = empty(metricsToken) ? [] : [
  { name: 'CLAVIGER_METRICS_TOKEN', secretRef: 'metrics-token' }
]
var otlpEnv = empty(otlpEndpoint) ? [] : [
  { name: 'OTEL_EXPORTER_OTLP_ENDPOINT', value: otlpEndpoint }
]
var auditEnv = auditRetentionDays > 0 ? [
  { name: 'CLAVIGER_AUDIT_RETENTION_DAYS', value: string(auditRetentionDays) }
] : []

var baseSecrets = [
  { name: 'encryption-key', value: encryptionKey }
]
var adminSecrets = empty(adminPassword) ? [] : [
  { name: 'admin-password', value: adminPassword }
]
var metricsSecrets = empty(metricsToken) ? [] : [
  { name: 'metrics-token', value: metricsToken }
]

resource app 'Microsoft.App/containerApps@2025-01-01' = {
  name: name
  location: location
  properties: {
    managedEnvironmentId: env.id
    workloadProfileName: 'Consumption'
    configuration: {
      // Single revision mode: the old revision is deactivated when the new one is ready.
      activeRevisionsMode: 'Single'
      secrets: concat(baseSecrets, adminSecrets, metricsSecrets)
      ingress: {
        external: true
        targetPort: 8090
        transport: 'auto'
        allowInsecure: false
      }
    }
    template: {
      terminationGracePeriodSeconds: 45
      containers: [
        {
          name: 'claviger'
          image: image
          resources: { cpu: json(cpu), memory: memory }
          env: concat(baseEnv, adminEnv, metricsEnv, otlpEnv, auditEnv)
          volumeMounts: [
            { volumeName: 'pbdata', mountPath: '/pb_data' }
          ]
          probes: [
            {
              type: 'Startup'
              httpGet: { path: '/api/health', port: 8090 }
              periodSeconds: 5
              failureThreshold: 36
            }
            {
              type: 'Readiness'
              httpGet: { path: '/api/health', port: 8090 }
              periodSeconds: 10
              timeoutSeconds: 5
              failureThreshold: 3
            }
            {
              type: 'Liveness'
              httpGet: { path: '/api/health', port: 8090 }
              periodSeconds: 30
              timeoutSeconds: 5
              failureThreshold: 5
            }
          ]
        }
      ]
      volumes: [
        {
          name: 'pbdata'
          storageType: 'NfsAzureFile'
          storageName: envStorage.name
        }
      ]
      // SQLite is single-writer. Never raise maxReplicas.
      scale: {
        minReplicas: 1
        maxReplicas: 1
      }
    }
  }
}

output url string = 'https://${app.properties.configuration.ingress.fqdn}'
output storageAccount string = storage.name
