package account

import (
	"time"

	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/spf13/cobra"
)

// New constructs the account commands with command-scoped client and target resolution.
func New(factory cliutil.ClientFactory[Client], remoteURL, hostDestination func(*cobra.Command) string) *cobra.Command {
	g := &group{newClient: factory, remoteURL: remoteURL, hostDestination: hostDestination}
	account := &cobra.Command{
		Use:   "account",
		Short: "Manage agent accounts (credential registry)",
	}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List accounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runAccountLS(cmd)
		},
	}
	ls.Flags().String("provider", "", "Filter by provider (claude|codex)")
	ls.Flags().Bool("json", false, "Emit a stable JSON schema instead of a table")
	ls.Flags().Bool("refresh", false, "Force a live usage probe of each account before listing")

	add := &cobra.Command{
		Use:   "add [provider]",
		Short: "Register an agent account",
		Args:  cobra.MaximumNArgs(1),
		RunE:  g.runAccountAddDispatch,
	}
	add.Flags().String("provider", "", "Account provider ("+trimmedProviderList()+") (or pass as a positional arg)")
	add.Flags().String("label", "", "Human label, unique per provider (required for the non-interactive flag path)")
	add.Flags().Int32("priority", 0, "Sort order; lower = preferred")
	// Credential hygiene: prefer stdin/env over --token so the secret does not
	// land in shell history.
	add.Flags().String("token", "", "Credential token (prefer --credential-file - or stdin to keep it out of shell history)")
	add.Flags().String("credential-file", "", "Read the credential from a file (or '-' for stdin); preferred over --token")
	// Interactive registration (claude setup-token walkthrough / codex device flow).
	add.Flags().Bool("token-stdin", false, "claude only: read the setup token from stdin instead of running the walkthrough")
	add.Flags().Duration("timeout", 10*time.Minute, "Deadline for an interactive registration walkthrough")

	refresh := &cobra.Command{
		Use:   "refresh <account-id>",
		Short: "Replace an account's stored credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runAccountRefresh(cmd, args[0])
		},
	}
	refresh.Flags().String("token", "", "Credential token (prefer --credential-file - or stdin to keep it out of shell history)")
	refresh.Flags().String("credential-file", "", "Read the credential from a file (or '-' for stdin); preferred over --token")
	refresh.Flags().Bool("test", false, "Validate the refreshed credential after saving")
	refresh.Flags().Bool("json", false, "Emit a stable JSON schema instead of text")

	update := &cobra.Command{
		Use:   "update <account-id>",
		Short: "Update account metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runAccountUpdate(cmd, args[0])
		},
	}
	update.Flags().String("label", "", "Set the label")
	update.Flags().Int32("priority", 0, "Set the priority (lower = preferred)")
	update.Flags().String("status", "", "Set the status (active|disabled)")
	update.Flags().StringSlice("allowed-models", nil, "Replace the allowed-models set (comma-separated)")

	// `reauth` is deliberately its own verb rather than a flag on `refresh`.
	// Refresh takes a credential you already have; reauth ACQUIRES one by
	// driving an interactive device login, so its flags, its stdin needs and
	// its failure modes are a different command's (BOS-1142).
	reauth := &cobra.Command{
		Use:   "reauth <account-id>",
		Short: "Re-run the provider login for an account and replace its credential in place",
		Long: "Drive the provider's interactive device login and store the resulting credential on the EXISTING account.\n\n" +
			"Use this when an account's credential was rejected: it keeps the account's id, label, priority and every\n" +
			"session binding, replacing only the secret, and verifies the replacement before reporting success.\n" +
			"Adding a new account instead would leave the failed row in place alongside it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runAccountReauth(cmd, args[0])
		},
	}
	reauth.Flags().Duration("timeout", 10*time.Minute, "Deadline for the interactive device login")

	account.AddCommand(
		ls,
		add,
		refresh,
		reauth,
		update,
		&cobra.Command{
			Use:   "remove <account-id>",
			Short: "Remove an account and its stored credential",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return g.runAccountRemove(cmd, args[0])
			},
		},
	)

	test := &cobra.Command{
		Use:   "test <account-id>",
		Short: "Validate an account's credential and record the outcome",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runAccountTest(cmd, args[0])
		},
	}
	test.Flags().Bool("json", false, "Emit a stable JSON schema instead of text")
	account.AddCommand(test)

	switchCmd := &cobra.Command{
		Use:   "switch <session> <account>",
		Short: "Stop a session's live chat, rebind it to the chosen account, and resume",
		Long: "Stop the session's live chat, rebind it to the chosen account, and respawn with resume.\n\n" +
			"<account> is an account id or label; pass \"system-default\" (or \"0\"/\"none\") to target the system default (account 0).\n" +
			"By default the session's primary live chat is switched; use --chat to target a specific agent chat.\n" +
			"A mid-turn (WORKING) chat is rejected unless --force is set.",
		Args: cobra.ExactArgs(2),
		RunE: g.runAccountSwitch,
	}
	switchCmd.Flags().String("chat", "", "Target a specific agent chat (agent session id); default: the session's primary live chat")
	switchCmd.Flags().Bool("force", false, "Interrupt a mid-turn / WORKING chat")
	account.AddCommand(switchCmd)

	return account
}
