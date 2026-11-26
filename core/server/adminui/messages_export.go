package adminui

// Messages of the data export (specs/070-company-export). Merged into the catalogue at start-up.
func init() {
	for k, v := range map[string]string{
		"nav.export": "Export",

		"ex.title":       "Export your data",
		"ex.help":        "Download everything the company holds here: the configuration and website, each module's records as CSV and JSON, files and audit logs. Passwords, tokens and keys are never included. Nothing is kept on the server.",
		"ex.slow":        "The download starts at once and can take a long time. Keep this page's sign-in until it has finished. If it breaks off, the file is not complete: start again. A complete export ends with manifest.json, whose warnings say what is not in it.",
		"ex.passphrase":  "Passphrase to encrypt the file (recommended)",
		"ex.passphrase2": "The passphrase again",
		"ex.pass_help":   "At least %d characters. It cannot be recovered. Open the file with: can export decrypt",
		"ex.sealed":      "Also decrypt the sealed fields (salaries, identity and bank account numbers, private notes, board materials)",
		"ex.sealed_warn": "With sealed fields the file holds the most sensitive data of the company in clear once opened. A passphrase is required.",
		"ex.understood":  "I understand, and I will keep the file safe and delete it when I am done",
		"ex.submit":      "Download the export",

		"ex.err_understood":        "Confirm that you understand what an export with sealed fields holds.",
		"ex.err_sealed_passphrase": "An export with sealed fields needs a passphrase.",
		"ex.err_mismatch":          "The two passphrases differ.",
		"ex.err_short":             "The passphrase is too short.",
		"ex.err_busy":              "An export or a backup dump is already running. Try again when it has finished.",
		"ex.err_failed":            "The export failed before anything was sent. The can-core log says why.",
	} {
		messagesEN[k] = v
	}
}
