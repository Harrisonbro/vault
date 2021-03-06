package command

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hashicorp/go-uuid"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/helper/password"
	"github.com/hashicorp/vault/helper/pgpkeys"
	"github.com/hashicorp/vault/helper/xor"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

var _ cli.Command = (*OperatorGenerateRootCommand)(nil)
var _ cli.CommandAutocomplete = (*OperatorGenerateRootCommand)(nil)

type OperatorGenerateRootCommand struct {
	*BaseCommand

	flagInit        bool
	flagCancel      bool
	flagStatus      bool
	flagDecode      string
	flagOTP         string
	flagPGPKey      string
	flagNonce       string
	flagGenerateOTP bool

	// Deprecation
	// TODO: remove in 0.9.0
	flagGenOTP bool

	testStdin io.Reader // for tests
}

func (c *OperatorGenerateRootCommand) Synopsis() string {
	return "Generates a new root token"
}

func (c *OperatorGenerateRootCommand) Help() string {
	helpText := `
Usage: vault operator generate-root [options] [KEY]

  Generates a new root token by combining a quorum of share holders. One of
  the following must be provided to start the root token generation:

    - A base64-encoded one-time-password (OTP) provided via the "-otp" flag.
      Use the "-generate-otp" flag to generate a usable value. The resulting
      token is XORed with this value when it is returned. Use the "-decode"
      flag to output the final value.

    - A file containing a PGP key or a keybase username in the "-pgp-key"
      flag. The resulting token is encrypted with this public key.

  An unseal key may be provided directly on the command line as an argument to
  the command. If key is specified as "-", the command will read from stdin. If
  a TTY is available, the command will prompt for text.

  Generate an OTP code for the final token:

      $ vault operator generate-root -generate-otp

  Start a root token generation:

      $ vault operator generate-root -init -otp="..."
      $ vault operator generate-root -init -pgp-key="..."

  Enter an unseal key to progress root token generation:

      $ vault operator generate-root -otp="..."

` + c.Flags().Help()
	return strings.TrimSpace(helpText)
}

func (c *OperatorGenerateRootCommand) Flags() *FlagSets {
	set := c.flagSet(FlagSetHTTP)

	f := set.NewFlagSet("Command Options")

	f.BoolVar(&BoolVar{
		Name:       "init",
		Target:     &c.flagInit,
		Default:    false,
		EnvVar:     "",
		Completion: complete.PredictNothing,
		Usage: "Start a root token generation. This can only be done if " +
			"there is not currently one in progress.",
	})

	f.BoolVar(&BoolVar{
		Name:       "cancel",
		Target:     &c.flagCancel,
		Default:    false,
		EnvVar:     "",
		Completion: complete.PredictNothing,
		Usage: "Reset the root token generation progress. This will discard any " +
			"submitted unseal keys or configuration.",
	})

	f.BoolVar(&BoolVar{
		Name:       "status",
		Target:     &c.flagStatus,
		Default:    false,
		EnvVar:     "",
		Completion: complete.PredictNothing,
		Usage: "Print the status of the current attempt without providing an " +
			"unseal key.",
	})

	f.StringVar(&StringVar{
		Name:       "decode",
		Target:     &c.flagDecode,
		Default:    "",
		EnvVar:     "",
		Completion: complete.PredictAnything,
		Usage: "Decode and output the generated root token. This option requires " +
			"the \"-otp\" flag be set to the OTP used during initialization.",
	})

	f.BoolVar(&BoolVar{
		Name:       "generate-otp",
		Target:     &c.flagGenerateOTP,
		Default:    false,
		EnvVar:     "",
		Completion: complete.PredictNothing,
		Usage: "Generate and print a high-entropy one-time-password (OTP) " +
			"suitable for use with the \"-init\" flag.",
	})

	f.StringVar(&StringVar{
		Name:       "otp",
		Target:     &c.flagOTP,
		Default:    "",
		EnvVar:     "",
		Completion: complete.PredictAnything,
		Usage:      "OTP code to use with \"-decode\" or \"-init\".",
	})

	f.VarFlag(&VarFlag{
		Name:       "pgp-key",
		Value:      (*pgpkeys.PubKeyFileFlag)(&c.flagPGPKey),
		Default:    "",
		EnvVar:     "",
		Completion: complete.PredictAnything,
		Usage: "Path to a file on disk containing a binary or base64-encoded " +
			"public GPG key. This can also be specified as a Keybase username " +
			"using the format \"keybase:<username>\". When supplied, the generated " +
			"root token will be encrypted and base64-encoded with the given public " +
			"key.",
	})

	f.StringVar(&StringVar{
		Name:       "nonce",
		Target:     &c.flagNonce,
		Default:    "",
		EnvVar:     "",
		Completion: complete.PredictAnything,
		Usage: "Nonce value provided at initialization. The same nonce value " +
			"must be provided with each unseal key.",
	})

	// Deprecations: prefer longer-form, descriptive flags
	// TODO: remove in 0.9.0
	f.BoolVar(&BoolVar{
		Name:    "genotp", // -generate-otp
		Target:  &c.flagGenOTP,
		Default: false,
		Hidden:  true,
	})

	return set
}

func (c *OperatorGenerateRootCommand) AutocompleteArgs() complete.Predictor {
	return nil
}

func (c *OperatorGenerateRootCommand) AutocompleteFlags() complete.Flags {
	return c.Flags().Completions()
}

func (c *OperatorGenerateRootCommand) Run(args []string) int {
	f := c.Flags()

	if err := f.Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	args = f.Args()
	if len(args) > 1 {
		c.UI.Error(fmt.Sprintf("Too many arguments (expected 0-1, got %d)", len(args)))
		return 1
	}

	// Deprecations
	// TODO: remove in 0.9.0
	switch {
	case c.flagGenOTP:
		c.UI.Warn(wrapAtLength(
			"The -gen-otp flag is deprecated. Please use the -generate-otp flag " +
				"instead."))
		c.flagGenerateOTP = c.flagGenOTP
	}

	client, err := c.Client()
	if err != nil {
		c.UI.Error(err.Error())
		return 2
	}

	switch {
	case c.flagGenerateOTP:
		return c.generateOTP()
	case c.flagDecode != "":
		return c.decode(c.flagDecode, c.flagOTP)
	case c.flagCancel:
		return c.cancel(client)
	case c.flagInit:
		return c.init(client, c.flagOTP, c.flagPGPKey)
	case c.flagStatus:
		return c.status(client)
	default:
		// If there are no other flags, prompt for an unseal key.
		key := ""
		if len(args) > 0 {
			key = strings.TrimSpace(args[0])
		}
		return c.provide(client, key)
	}
}

// verifyOTP verifies the given OTP code is exactly 16 bytes.
func (c *OperatorGenerateRootCommand) verifyOTP(otp string) error {
	if len(otp) == 0 {
		return fmt.Errorf("No OTP passed in")
	}
	otpBytes, err := base64.StdEncoding.DecodeString(otp)
	if err != nil {
		return fmt.Errorf("Error decoding base64 OTP value: %s", err)
	}
	if otpBytes == nil || len(otpBytes) != 16 {
		return fmt.Errorf("Decoded OTP value is invalid or wrong length")
	}

	return nil
}

// generateOTP generates a suitable OTP code for generating a root token.
func (c *OperatorGenerateRootCommand) generateOTP() int {
	buf := make([]byte, 16)
	readLen, err := rand.Read(buf)
	if err != nil {
		c.UI.Error(fmt.Sprintf("Error reading random bytes: %s", err))
		return 2
	}

	if readLen != 16 {
		c.UI.Error(fmt.Sprintf("Read %d bytes when we should have read 16", readLen))
		return 2
	}

	return PrintRaw(c.UI, base64.StdEncoding.EncodeToString(buf))
}

// decode decodes the given value using the otp.
func (c *OperatorGenerateRootCommand) decode(encoded, otp string) int {
	if encoded == "" {
		c.UI.Error("Missing encoded value: use -decode=<string> to supply it")
		return 1
	}
	if otp == "" {
		c.UI.Error("Missing otp: use -otp to supply it")
		return 1
	}

	tokenBytes, err := xor.XORBase64(encoded, otp)
	if err != nil {
		c.UI.Error(fmt.Sprintf("Error xoring token: %s", err))
		return 1
	}

	token, err := uuid.FormatUUID(tokenBytes)
	if err != nil {
		c.UI.Error(fmt.Sprintf("Error formatting base64 token value: %s", err))
		return 1
	}

	return PrintRaw(c.UI, strings.TrimSpace(token))
}

// init is used to start the generation process
func (c *OperatorGenerateRootCommand) init(client *api.Client, otp string, pgpKey string) int {
	// Validate incoming fields. Either OTP OR PGP keys must be supplied.
	switch {
	case otp == "" && pgpKey == "":
		c.UI.Error("Error initializing: must specify either -otp or -pgp-key")
		return 1
	case otp != "" && pgpKey != "":
		c.UI.Error("Error initializing: cannot specify both -otp and -pgp-key")
		return 1
	case otp != "":
		if err := c.verifyOTP(otp); err != nil {
			c.UI.Error(fmt.Sprintf("Error initializing: invalid OTP: %s", err))
			return 1
		}
	case pgpKey != "":
		// OK
	}

	// Start the root generation
	status, err := client.Sys().GenerateRootInit(otp, pgpKey)
	if err != nil {
		c.UI.Error(fmt.Sprintf("Error initializing root generation: %s", err))
		return 2
	}
	return c.printStatus(status)
}

// provide prompts the user for the seal key and posts it to the update root
// endpoint. If this is the last unseal, this function outputs it.
func (c *OperatorGenerateRootCommand) provide(client *api.Client, key string) int {
	status, err := client.Sys().GenerateRootStatus()
	if err != nil {
		c.UI.Error(fmt.Sprintf("Error getting root generation status: %s", err))
		return 2
	}

	// Verify a root token generation is in progress. If there is not one in
	// progress, return an error instructing the user to start one.
	if !status.Started {
		c.UI.Error(wrapAtLength(
			"No root generation is in progress. Start a root generation by " +
				"running \"vault generate-root -init\"."))
		return 1
	}

	var nonce string

	switch key {
	case "-": // Read from stdin
		nonce = c.flagNonce

		// Pull our fake stdin if needed
		stdin := (io.Reader)(os.Stdin)
		if c.testStdin != nil {
			stdin = c.testStdin
		}

		var buf bytes.Buffer
		if _, err := io.Copy(&buf, stdin); err != nil {
			c.UI.Error(fmt.Sprintf("Failed to read from stdin: %s", err))
			return 1
		}

		key = buf.String()
	case "": // Prompt using the tty
		// Nonce value is not required if we are prompting via the terminal
		nonce = status.Nonce

		w := getWriterFromUI(c.UI)
		fmt.Fprintf(w, "Root generation operation nonce: %s\n", nonce)
		fmt.Fprintf(w, "Unseal Key (will be hidden): ")
		key, err = password.Read(os.Stdin)
		fmt.Fprintf(w, "\n")
		if err != nil {
			if err == password.ErrInterrupted {
				c.UI.Error("user canceled")
				return 1
			}

			c.UI.Error(wrapAtLength(fmt.Sprintf("An error occurred attempting to "+
				"ask for the unseal key. The raw error message is shown below, but "+
				"usually this is because you attempted to pipe a value into the "+
				"command or you are executing outside of a terminal (tty). If you "+
				"want to pipe the value, pass \"-\" as the argument to read from "+
				"stdin. The raw error was: %s", err)))
			return 1
		}
	default: // Supplied directly as an arg
		nonce = c.flagNonce
	}

	// Trim any whitespace from they key, especially since we might have prompted
	// the user for it.
	key = strings.TrimSpace(key)

	// Verify we have a nonce value
	if nonce == "" {
		c.UI.Error("Missing nonce value: specify it via the -nonce flag")
		return 1
	}

	// Provide the key, this may potentially complete the update
	status, err = client.Sys().GenerateRootUpdate(key, nonce)
	if err != nil {
		c.UI.Error(fmt.Sprintf("Error posting unseal key: %s", err))
		return 2
	}
	return c.printStatus(status)
}

// cancel cancels the root token generation
func (c *OperatorGenerateRootCommand) cancel(client *api.Client) int {
	if err := client.Sys().GenerateRootCancel(); err != nil {
		c.UI.Error(fmt.Sprintf("Error canceling root token generation: %s", err))
		return 2
	}
	c.UI.Output("Success! Root token generation canceled (if it was started)")
	return 0
}

// status is used just to fetch and dump the status
func (c *OperatorGenerateRootCommand) status(client *api.Client) int {
	status, err := client.Sys().GenerateRootStatus()
	if err != nil {
		c.UI.Error(fmt.Sprintf("Error getting root generation status: %s", err))
		return 2
	}
	return c.printStatus(status)
}

// printStatus dumps the status to output
func (c *OperatorGenerateRootCommand) printStatus(status *api.GenerateRootStatusResponse) int {
	out := []string{}
	out = append(out, fmt.Sprintf("Nonce | %s", status.Nonce))
	out = append(out, fmt.Sprintf("Started | %t", status.Started))
	out = append(out, fmt.Sprintf("Progress | %d/%d", status.Progress, status.Required))
	out = append(out, fmt.Sprintf("Complete | %t", status.Complete))
	if status.PGPFingerprint != "" {
		out = append(out, fmt.Sprintf("PGP Fingerprint | %s", status.PGPFingerprint))
	}
	if status.EncodedRootToken != "" {
		out = append(out, fmt.Sprintf("Root Token | %s", status.EncodedRootToken))
	}

	output := columnOutput(out, nil)
	c.UI.Output(output)
	return 0
}
