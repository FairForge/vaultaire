# OneDrive Integration Setup

## Step 152: Azure AD App Registration

### Prerequisites
- Azure account with access to Azure Active Directory
- Admin consent for Microsoft Graph API permissions

### Registration Steps
1. Go to Azure Portal > Azure Active Directory > App registrations
2. Click "New registration"
3. Name: "Vaultaire Storage Platform"
4. Supported account types: "Accounts in this organizational directory only"
5. Redirect URI: Leave blank for now (client credentials flow)

### API Permissions Required
- Microsoft Graph:
  - Files.ReadWrite.All (Application)
  - Sites.ReadWrite.All (Application)
  - User.Read.All (Application)

### Client Credentials
1. Go to Certificates & secrets
2. Create new client secret
3. Store securely:
   - Client ID: ___________
   - Client Secret: ___________
   - Tenant ID: ___________

### Testing
Use environment variables:
export ONEDRIVE_CLIENT_ID="..."
export ONEDRIVE_CLIENT_SECRET="..."
export ONEDRIVE_TENANT_ID="..."
