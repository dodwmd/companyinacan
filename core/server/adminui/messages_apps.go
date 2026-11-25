package adminui

// Messages of the Apps launcher (E11c, specs/036-module-ui-sso).
func init() {
	for k, v := range map[string]string{
		"nav.apps":         "Apps",
		"apps.title":       "Apps",
		"apps.help":        "Open a module's own web page already signed in as you. You get exactly the access your roles give you in that module, and you can keep working there until you sign out or your access changes.",
		"apps.open":        "Open",
		"apps.open_label":  "Open %s",
		"apps.none":        "No apps are available to you. A module appears here when it has a web page, is enabled, and your roles give you a permission in it.",
		"apps.opening":     "Opening the app",
		"apps.refused":     "App not available",
		"apps.refused_msg": "That app does not exist or your roles do not give you access to it.",
		"apps.address":     "The app cannot be opened from this address",
		"apps.address_msg": "Open the admin UI at its configured address (on the same domain as the app) and try again.",
		"apps.busy":        "Too many launches",
		"apps.busy_msg":    "You opened apps many times in the last minute. Wait a minute and try again.",
	} {
		messagesEN[k] = v
	}
}
