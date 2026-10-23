// VNet + dedicated subnet for the Container Apps workload-profile environment.
// The subnet is delegated to Microsoft.App/environments (required for vnetConfiguration.infrastructureSubnetId)
// and has a service endpoint to Microsoft.Storage so the Premium FileStorage account
// can scope its firewall to this subnet for NFS mounts.

@description('Name of the VNet.')
param vnetName string

@description('Name of the subnet that hosts the Container Apps environment.')
param subnetName string

@description('Region for VNet resources.')
param location string

@description('VNet address space (CIDR).')
param vnetAddressPrefix string = '10.40.0.0/23'

@description('Subnet CIDR. Workload-profile Container Apps environments require /27 minimum.')
param subnetAddressPrefix string = '10.40.0.0/27'

resource vnet 'Microsoft.Network/virtualNetworks@2024-01-01' = {
  name: vnetName
  location: location
  properties: {
    addressSpace: {
      addressPrefixes: [vnetAddressPrefix]
    }
    subnets: [
      {
        name: subnetName
        properties: {
          addressPrefix: subnetAddressPrefix
          delegations: [
            {
              name: 'Microsoft.App.environments'
              properties: {
                serviceName: 'Microsoft.App/environments'
              }
            }
          ]
          serviceEndpoints: [
            {
              service: 'Microsoft.Storage'
              locations: [location]
            }
          ]
        }
      }
    ]
  }
}

output vnetId string = vnet.id
output subnetId string = '${vnet.id}/subnets/${subnetName}'
