package chatpicker

// ExternalActions connects local browser and terminal operations at the application boundary.
type ExternalActions struct {
	SupportsNewTab bool
	ChatTitle      func(string, string) string
	OpenNewTab     func(string) error
	OpenURL        func(string) error
}
