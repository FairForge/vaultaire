package rbac

import (
	"strings"
	"sync"
)

// Permission represents a specific action that can be performed
type Permission string

// Permission categories and specific permissions
const (
	// Storage permissions
	PermStorageRead   Permission = "storage.read"
	PermStorageWrite  Permission = "storage.write"
	PermStorageDelete Permission = "storage.delete"
	PermStorageList   Permission = "storage.list"

	// Bucket permissions
	PermBucketCreate Permission = "bucket.create"
	PermBucketDelete Permission = "bucket.delete"
	PermBucketList   Permission = "bucket.list"
	PermBucketRead   Permission = "bucket.read"

	// User permissions
	PermUserRead   Permission = "user.read"
	PermUserWrite  Permission = "user.write"
	PermUserDelete Permission = "user.delete"

	// Profile permissions
	PermProfileRead  Permission = "profile.read"
	PermProfileWrite Permission = "profile.write"

	// API Key permissions
	PermAPIKeyCreate Permission = "apikey.create"
	PermAPIKeyDelete Permission = "apikey.delete"
	PermAPIKeyList   Permission = "apikey.list"

	// Admin permissions
	PermAdminUsers   Permission = "admin.users"
	PermAdminBilling Permission = "admin.billing"
	PermAdminSystem  Permission = "admin.system"
	PermAdminReports Permission = "admin.reports"

	// Auth permissions
	PermAuthLogin    Permission = "auth.login"
	PermAuthRegister Permission = "auth.register"
	PermAuthLogout   Permission = "auth.logout"

	// Billing permissions
	PermBillingRead   Permission = "billing.read"
	PermBillingUpdate Permission = "billing.update"

	// Quota permissions
	PermQuotaRead   Permission = "quota.read"
	PermQuotaUpdate Permission = "quota.update"
)

// PermissionSet is a set of permissions
type PermissionSet map[Permission]bool

// PermissionMatrix maps roles to their permissions
type PermissionMatrix map[string]PermissionSet

var (
	defaultPermissions = map[Permission]bool{
		// Storage
		PermStorageRead:   true,
		PermStorageWrite:  true,
		PermStorageDelete: true,
		PermStorageList:   true,

		// Buckets
		PermBucketCreate: true,
		PermBucketDelete: true,
		PermBucketList:   true,
		PermBucketRead:   true,

		// Users
		PermUserRead:   true,
		PermUserWrite:  true,
		PermUserDelete: true,

		// Profile
		PermProfileRead:  true,
		PermProfileWrite: true,

		// API Keys
		PermAPIKeyCreate: true,
		PermAPIKeyDelete: true,
		PermAPIKeyList:   true,

		// Admin
		PermAdminUsers:   true,
		PermAdminBilling: true,
		PermAdminSystem:  true,
		PermAdminReports: true,

		// Auth
		PermAuthLogin:    true,
		PermAuthRegister: true,
		PermAuthLogout:   true,

		// Billing
		PermBillingRead:   true,
		PermBillingUpdate: true,

		// Quota
		PermQuotaRead:   true,
		PermQuotaUpdate: true,
	}

	// Default permission matrix
	defaultMatrix = PermissionMatrix{
		RoleAdmin: {
			// Admin has all permissions
			PermStorageRead:   true,
			PermStorageWrite:  true,
			PermStorageDelete: true,
			PermStorageList:   true,
			PermBucketCreate:  true,
			PermBucketDelete:  true,
			PermBucketList:    true,
			PermBucketRead:    true,
			PermUserRead:      true,
			PermUserWrite:     true,
			PermUserDelete:    true,
			PermProfileRead:   true,
			PermProfileWrite:  true,
			PermAPIKeyCreate:  true,
			PermAPIKeyDelete:  true,
			PermAPIKeyList:    true,
			PermAdminUsers:    true,
			PermAdminBilling:  true,
			PermAdminSystem:   true,
			PermAdminReports:  true,
			PermAuthLogin:     true,
			PermAuthRegister:  true,
			PermAuthLogout:    true,
			PermBillingRead:   true,
			PermBillingUpdate: true,
			PermQuotaRead:     true,
			PermQuotaUpdate:   true,
		},
		RoleUser: {
			// Standard user permissions
			PermStorageRead:   true,
			PermStorageWrite:  true,
			PermStorageDelete: true,
			PermStorageList:   true,
			PermBucketCreate:  true,
			PermBucketDelete:  true,
			PermBucketList:    true,
			PermBucketRead:    true,
			PermUserRead:      true,
			PermUserWrite:     true,
			PermProfileRead:   true,
			PermProfileWrite:  true,
			PermAPIKeyCreate:  true,
			PermAPIKeyDelete:  true,
			PermAPIKeyList:    true,
			PermAuthLogin:     true,
			PermAuthLogout:    true,
			PermBillingRead:   true,
			PermBillingUpdate: true,
			PermQuotaRead:     true,
		},
		RoleViewer: {
			// Read-only permissions
			PermStorageRead: true,
			PermStorageList: true,
			PermBucketList:  true,
			PermBucketRead:  true,
			PermUserRead:    true,
			PermProfileRead: true,
			PermAPIKeyList:  true,
			PermAuthLogin:   true,
			PermAuthLogout:  true,
			PermBillingRead: true,
			PermQuotaRead:   true,
		},
		RoleGuest: {
			// Minimal permissions
			PermAuthLogin:    true,
			PermAuthRegister: true,
		},
	}
)

// GetDefaultPermissions returns all available permissions
func GetDefaultPermissions() map[Permission]bool {
	perms := make(map[Permission]bool)
	for perm := range defaultPermissions {
		perms[perm] = true
	}
	return perms
}

// GetPermissionsByCategory returns permissions for a specific category
func GetPermissionsByCategory(category string) []Permission {
	var perms []Permission
	prefix := category + "."

	for perm := range defaultPermissions {
		if strings.HasPrefix(string(perm), prefix) {
			perms = append(perms, perm)
		}
	}

	return perms
}

// IsValidPermission checks if a permission string is valid
func IsValidPermission(perm string) bool {
	if perm == "" || !strings.Contains(perm, ".") {
		return false
	}

	// Check for double dots or leading/trailing dots
	if strings.Contains(perm, "..") ||
		strings.HasPrefix(perm, ".") ||
		strings.HasSuffix(perm, ".") {
		return false
	}

	_, exists := defaultPermissions[Permission(perm)]
	return exists
}

// GetDefaultPermissionMatrix returns the default role-permission mappings
func GetDefaultPermissionMatrix() PermissionMatrix {
	// Return a copy to prevent modification
	matrix := make(PermissionMatrix)
	for role, perms := range defaultMatrix {
		permCopy := make(PermissionSet)
		for perm, allowed := range perms {
			permCopy[perm] = allowed
		}
		matrix[role] = permCopy
	}
	return matrix
}

// PermissionChecker handles permission checks
type PermissionChecker struct {
	matrix PermissionMatrix
	mu     sync.RWMutex
}

// NewPermissionChecker creates a new permission checker
func NewPermissionChecker() *PermissionChecker {
	return &PermissionChecker{
		matrix: GetDefaultPermissionMatrix(),
	}
}

// HasPermission checks if a role has a specific permission
func (pc *PermissionChecker) HasPermission(role string, permission string) bool {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	rolePerms, exists := pc.matrix[role]
	if !exists {
		return false
	}

	return rolePerms[Permission(permission)]
}

// HasAllPermissions checks if a role has all specified permissions (AND)
func (pc *PermissionChecker) HasAllPermissions(role string, permissions []string) bool {
	for _, perm := range permissions {
		if !pc.HasPermission(role, perm) {
			return false
		}
	}
	return true
}

// HasAnyPermission checks if a role has any of the specified permissions (OR)
func (pc *PermissionChecker) HasAnyPermission(role string, permissions []string) bool {
	for _, perm := range permissions {
		if pc.HasPermission(role, perm) {
			return true
		}
	}
	return false
}
