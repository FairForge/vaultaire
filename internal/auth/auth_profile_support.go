package auth

// InitProfiles initializes the profiles map
func (a *AuthService) InitProfiles() {
	if a.profiles == nil {
		a.profiles = make(map[string]*ProfileUpdate)
	}
}
