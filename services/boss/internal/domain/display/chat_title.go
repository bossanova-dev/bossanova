package display

// IsPlaceholderChatTitle reports whether a stored chat title needs transcript backfill.
func IsPlaceholderChatTitle(title string) bool { return title == "" || title == "New chat" }
