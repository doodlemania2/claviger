// Premium FileStorage account with NFS file share, scoped to a Container Apps subnet.
// NFS Azure Files requires:
//   - sku Premium_LRS + kind FileStorage (Standard tier does not support NFS)
//   - The mounting subnet has a Microsoft.Storage service endpoint
//   - networkAcls denies by default and explicitly allows the subnet
//   - supportsHttpsTrafficOnly = false (NFS is unencrypted; HTTPS-only rejects the mount)
//   - Share enabledProtocols = NFS with no root squash so the container UID can write

@description('Storage account name (must be globally unique, 3-24 lowercase alphanumeric)')
param name string

@description('Location for the storage account')
param location string

@description('Tags for the resource')
param tags object = {}

@description('Resource ID of the subnet permitted to mount the NFS share')
param subnetId string

@description('NFS file share quota in GiB. Premium FileStorage minimum is 100.')
@minValue(100)
param shareQuotaGiB int = 100

var fileShareName = 'pbdata'

resource storageAccount 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: name
  location: location
  tags: tags
  sku: {
    name: 'Premium_LRS'
  }
  kind: 'FileStorage'
  properties: {
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: false
    allowBlobPublicAccess: false
    publicNetworkAccess: 'Enabled'
    networkAcls: {
      defaultAction: 'Deny'
      bypass: 'AzureServices'
      virtualNetworkRules: [
        {
          id: subnetId
          action: 'Allow'
        }
      ]
    }
  }
}

resource fileService 'Microsoft.Storage/storageAccounts/fileServices@2023-05-01' = {
  parent: storageAccount
  name: 'default'
}

resource fileShare 'Microsoft.Storage/storageAccounts/fileServices/shares@2023-05-01' = {
  parent: fileService
  name: fileShareName
  properties: {
    enabledProtocols: 'NFS'
    rootSquash: 'NoRootSquash'
    shareQuota: shareQuotaGiB
  }
}

output storageAccountName string = storageAccount.name
output storageAccountId string = storageAccount.id
output fileShareName string = fileShare.name
output nfsServer string = '${storageAccount.name}.file.${environment().suffixes.storage}'
output nfsShareName string = '/${storageAccount.name}/${fileShareName}'
