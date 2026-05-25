package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/spf13/cobra"
)

const maxWispAutocloseHookPayloadBytes = 16 << 20

func newWispCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "wisp",
		Short:  "Wisp lifecycle operations",
		Hidden: true,
	}
	cmd.AddCommand(newWispAutocloseCmd(stdout, stderr))
	return cmd
}

func newWispAutocloseCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    "autoclose <bead-id>",
		Short:  "Auto-close open molecule descendants of a closed bead",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			doWispAutoclose(args[0], cmd.InOrStdin(), stdout, stderr)
			return nil // always succeed — best-effort infrastructure
		},
	}
}

// doWispAutoclose is the CLI entry point for wisp autoclose.
// It resolves the current store through the provider-aware resolver using the
// projected store-root environment and delegates to the testable core.
func doWispAutoclose(beadID string, stdin io.Reader, stdout, _ io.Writer) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	storeRoot := convoyAutocloseStoreRoot(cwd)
	cityPath := autocloseCityPathForStoreRoot(storeRoot)
	store, err := openStoreAtForCity(storeRoot, cityPath)
	if err != nil {
		return
	}
	if parent, ok := wispAutocloseHookBead(stdin, beadID); ok {
		doWispAutocloseWithHookBead(store, parent, beadID, stdout)
		return
	}
	doWispAutocloseWith(store, beadID, stdout)
}

// doWispAutocloseWith closes any open attached molecule/workflow roots and
// their descendants for the given bead. Metadata-based attachments are
// preferred, with child traversal as a fallback for legacy data. Called from
// the bd on_close hook to ensure attached wisps don't outlive their parent work
// bead. All errors are silently swallowed — this is best-effort infrastructure.
func doWispAutocloseWith(store beads.Store, beadID string, stdout io.Writer) {
	parent, err := store.Get(beadID)
	if err != nil {
		return
	}
	doWispAutocloseWithHookBead(store, parent, beadID, stdout)
}

func doWispAutocloseWithHookBead(store beads.Store, parent beads.Bead, beadID string, stdout io.Writer) {
	if strings.TrimSpace(parent.ID) == "" {
		return
	}
	if strings.TrimSpace(beadID) == "" {
		beadID = parent.ID
	}
	attachments, err := collectAttachedBeads(parent, store, store)
	if err != nil && len(attachments) == 0 {
		return
	}
	for _, attached := range attachments {
		closed, err := closeAttachedWispSubtree(store, attached)
		if err != nil || closed == 0 {
			continue
		}
		fmt.Fprintf(stdout, "Auto-closed %s %s on %s\n", attachmentLabel(attached), attached.ID, beadID) //nolint:errcheck // best-effort stdout
	}
}

func wispAutocloseHookBead(stdin io.Reader, beadID string) (beads.Bead, bool) {
	if stdin == nil {
		return beads.Bead{}, false
	}
	if file, ok := stdin.(*os.File); ok {
		info, err := file.Stat()
		if err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return beads.Bead{}, false
		}
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxWispAutocloseHookPayloadBytes))
	if err != nil {
		return beads.Bead{}, false
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return beads.Bead{}, false
	}

	var parent beads.Bead
	if err := json.Unmarshal(data, &parent); err == nil && wispAutocloseHookBeadMatches(parent, beadID) {
		return parent, true
	}
	var parents []beads.Bead
	if err := json.Unmarshal(data, &parents); err == nil && len(parents) > 0 && wispAutocloseHookBeadMatches(parents[0], beadID) {
		return parents[0], true
	}
	return beads.Bead{}, false
}

func wispAutocloseHookBeadMatches(parent beads.Bead, beadID string) bool {
	parentID := strings.TrimSpace(parent.ID)
	if parentID == "" {
		return false
	}
	beadID = strings.TrimSpace(beadID)
	return beadID == "" || parentID == beadID
}

func closeAttachedWispSubtree(store beads.Store, attached beads.Bead) (int, error) {
	return sling.CloseAttachedSubtree(store, attached)
}
