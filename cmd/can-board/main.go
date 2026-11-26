// Command can-board is the in-house board and entity service: the `board` module (bodies, meetings,
// agendas and packs, quorum, conflicts, resolutions with roll-call votes, written consent and secret
// ballot, minutes, action items) and the `entity` module (legal entities, registered agent and
// addresses, statutory registers, filings calendar). See modules/board, modules/entity, docs/board.md
// and docs/entity.md.
//
// This is a tool, not legal advice.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/board"
	"github.com/dodwmd/companyinacan/core/modbackup"
)

func main() {
	if modbackup.Handles(os.Args[1:]) { // `backup` and `restore` (specs/043-module-backup-hooks)
		os.Exit(modbackup.Main("can-board", board.Migrations(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
	}
	os.Exit(board.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
