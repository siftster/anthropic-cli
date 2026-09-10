package cmd

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestSessionsConnectRegistered(t *testing.T) {
	i := slices.IndexFunc(Command.Commands, func(c *cli.Command) bool { return c.Name == "beta:sessions" })
	require.GreaterOrEqual(t, i, 0, "beta:sessions group missing from the generated tree")
	require.Contains(t, Command.Commands[i].Commands, &sessionsConnect)
}
