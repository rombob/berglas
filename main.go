// Copyright 2019 The Berglas Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/GoogleCloudPlatform/berglas/pkg/berglas"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const (
	// APIExitCode is the exit code returned with an upstream API call fails.
	APIExitCode = 60

	// MisuseExitCode is the exit code returned when the user did something wrong
	// such as misused a flag.
	MisuseExitCode = 61
)

var (
	stdout = os.Stdout
	stderr = os.Stderr
	stdin  = os.Stdin

	logFormat string
	logLevel  string

	accessGeneration int64

	listGenerations bool
	listPrefix      string

	key       string
	execLocal bool

	editor          string
	createIfMissing bool

	members []string

	projectID      string
	bucket         string
	bucketLocation string
	kmsLocation    string
	kmsKeyRing     string
	kmsCryptoKey   string
)

var rootCmd = &cobra.Command{
	Use:   "berglas",
	Short: "Interact with encrypted secrets",
	Long: strings.Trim(`
berglas is a CLI tool to reading, writing, and deleting secrets from a Cloud
Storage bucket encrypted with a Google Cloud KMS key. Secrets are encrypted
locally using envelope encryption before being uploaded to Cloud Storage.

Secrets are specified in the format:

    <bucket>/<secret>

For example:

    my-gcs-bucket/my-secret
    my-gcs-bucket/foo/bar/baz

For more information and examples, see the help text for a specific command.
`, "\n"),
	SilenceErrors: true,
	SilenceUsage:  true,
	Version:       berglas.Version,
}

var accessCmd = &cobra.Command{
	Use:   "access SECRET",
	Short: "Access a secret's contents",
	Long: strings.Trim(`
Accesses the contents of a secret by reading the encrypted data from Google
Cloud Storage and decrypting it with Google Cloud KMS.

The result will be the raw value without any additional formatting or newline
characters.
`, "\n"),
	Example: strings.Trim(`
  # Read a secret named "api-key" from the bucket "my-secrets"
  berglas access my-secrets/api-key

  # Read generation 1563925940580201 of a secret named "api-key" from the bucket "my-secrets"
  berglas access my-secrets/api-key --generation 1563925940580201
`, "\n"),
	Args: cobra.ExactArgs(1),
	RunE: accessRun,
}

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Bootstrap a berglas environment",
	Long: strings.Trim(`
Bootstrap a Berglas environment by creating a Cloud Storage bucket and a Cloud
KMS key with properly scoped permissions to the caller.

This command will create a new Cloud Storage bucket with "private" ACLs and
grant permission only to the caller in the specified project. It will enable
versioning on the bucket, configured to retain the last 10 verions. If the
bucket already exists, an error is returned.

This command will also create a Cloud KMS key ring and crypto key in the
specified project. If the key ring or crypto key already exist, no errors are
returned.
`, "\n"),
	Example: strings.Trim(`
  # Bootstrap a berglas environment
  berglas bootstrap --project my-project --bucket my-bucket
`, "\n"),
	Args: cobra.ExactArgs(0),
	RunE: bootstrapRun,
}

var createCmd = &cobra.Command{
	Use:   "create SECRET DATA",
	Short: "Create a secret",
	Long: strings.Trim(`
Creates a new secret with the given name and contents, encrypted with the
provided Cloud KMS key. If the secret already exists, an error is returned.

Use the "edit" or "update" commands to update an existing secret.
`, "\n"),
	Example: strings.Trim(`
  # Create a secret named "api-key" with the contents "abcd1234"
  berglas create my-secrets/api-key abcd1234 \
    --key projects/my-p/locations/global/keyRings/my-kr/cryptoKeys/my-k

  # Read a secret from stdin
  echo ${SECRET} | berglas create my-secrets/api-key - --key...

  # Read a secret from a local file
  berglas create my-secrets/api-key @/path/to/file --key...
`, "\n"),
	Args: cobra.ExactArgs(2),
	RunE: createRun,
}

var deleteCmd = &cobra.Command{
	Use:   "delete SECRET",
	Short: "Remove a secret",
	Long: strings.Trim(`
Deletes a secret from a Google Cloud Storage bucket by deleting the underlying
GCS object. If the secret does not exist, this operation is a no-op.

This command will exit successfully even if the secret does not exist.
`, "\n"),
	Example: strings.Trim(`
  # Delete a secret named "api-key"
  berglas delete my-secrets/api-key
`, "\n"),
	Args: cobra.ExactArgs(1),
	RunE: deleteRun,
}

var editCmd = &cobra.Command{
	Use:   "edit SECRET",
	Short: "Edit an existing secret",
	Long: strings.Trim(`
Updates the contents of an existing secret by reading the encrypted data from
Google Cloud Storage, decrypting it with Google Cloud KMS, editing it in-place
using an editor, encrypting the updated content using Google Cloud KMS, writing
it back into Google Cloud Storage.

The file must be saved with changes and editor must exit with exit code 0 for
the secret to be updated.
`, "\n"),
	Example: strings.Trim(`
  # Edit a secret named "api-key" from the bucket "my-secrets"
  berglas edit my-secrets/api-key

  # Edit a secret named "api-key" from the bucket "my-secrets" using emacs
  berglas edit my-secrets/api-key --editor emacs
`, "\n"),
	Args: cobra.ExactArgs(1),
	RunE: editRun,
}

var execCmd = &cobra.Command{
	Use:   "exec -- SUBCOMMAND",
	Short: "Spawn an environment with secrets",
	Long: strings.Trim(`
Parse berglas references and spawn the given command with the secrets in the
childprocess environment similar to exec(1). This is very useful in Docker
containers or languages that do not support auto-import.

By default, this command attempts to communicate with the Cloud APIs to find the
list of environment variables set on a resource. If you are not running inside a
supported runtime, you can specify "-local" to parse the local environment
variables instead.

Berglas will remain the parent process, but stdin, stdout, stderr, and any
signals are proxied to the child process.

WARNING: Using berglas exec exposes secrets in plaintext in environment
variables. You should have a strong understanding of your software supply
chain security before blindly running a process with berglas exec. The
resolved secrets will be in plaintext and available to the entire process.
`, "\n"),
	Example: strings.Trim(`
  # Spawn a subshell with secrets populated
  berglas exec -- ${SHELL}

  # Run "myapp" after parsing local references
  berglas exec --local -- myapp --with-args
`, "\n"),
	Args: cobra.MinimumNArgs(1),
	RunE: execRun,
}

var grantCmd = &cobra.Command{
	Use:   "grant SECRET",
	Short: "Grant access to a secret",
	Long: strings.Trim(`
Grant IAM access to an existing secret for a given list of members. The secret
must exist before access can be granted.

When executed, this command grants each specified member two IAM permissions:

  - roles/storage.legacyObjectReader on the Cloud Storage object
  - roles/cloudkms.cryptoKeyDecrypter on the Cloud KMS crypto key

Members must be specified with their type, for example:

  - domain:mydomain.com
  - group:group@mydomain.com
  - serviceAccount:xyz@gserviceaccount.com
  - user:user@mydomain.com
`, "\n"),
	Example: strings.Trim(`
  # Grant access to a user
  berglas grant my-secrets/api-key --member user:user@mydomain.com

  # Grant access to service account
  berglas grant my-secrets/api-key \
    --member serviceAccount:sa@project.iam.gserviceaccount.com

  # Add multiple members
  berglas grant my-secrets/api-key \
    --member user:user@mydomain.com \
    --member serviceAccount:sa@project.iam.gserviceaccount.com
`, "\n"),
	Args: cobra.ExactArgs(1),
	RunE: grantRun,
}

var listCmd = &cobra.Command{
	Use:   "list BUCKET",
	Short: "List secrets in a bucket",
	Long: strings.Trim(`
Lists secrets by name in the given Google Cloud Storage bucket. It does not
read their values, only their key names. To retrieve the value of a secret, use
the "access" command instead.
`, "\n"),
	Example: strings.Trim(`
  # List all secrets in the bucket "my-secrets"
  berglas list my-secrets

  # List all secrets with names starting with "secret" in the bucket "my-secrets"
  berglas list my-secrets --prefix secret

  # List all generations of all secrets in the bucket "my-secrets"
  berglas list my-secrets --all-generations
`, "\n"),
	Args: cobra.ExactArgs(1),
	RunE: listRun,
}

var revokeCmd = &cobra.Command{
	Use:   "revoke SECRET",
	Short: "Revoke access to a secret",
	Long: strings.Trim(`
Revoke IAM access to an existing secret for a given list of members. The secret
must exist for access to be revoked.

When executed, this command revokes the following IAM permissions for each
member:

  - roles/storage.legacyObjectReader on the Cloud Storage object
  - roles/cloudkms.cryptoKeyDecrypter on the Cloud KMS crypto key

If the member is not granted the IAM permissions, no action is taken.
Specifically, this does not return an error if the member did not originally
have permission to access the secret.

Members must be specified with their type, for example:

  - domain:mydomain.com
  - group:group@mydomain.com
  - serviceAccount:xyz@gserviceaccount.com
  - user:user@mydomain.com
`, "\n"),
	Example: strings.Trim(`
  # Revoke access from a user
  berglas revoke my-secrets/api-key --member user:user@mydomain.com

  # Revoke revoke from a service account
  berglas grant my-secrets/api-key \
    --member serviceAccount:sa@project.iam.gserviceaccount.com

  # Remove multiple members
  berglas revoke my-secrets/api-key \
    --member user:user@mydomain.com \
    --member serviceAccount:sa@project.iam.gserviceaccount.com
`, "\n"),
	Args: cobra.ExactArgs(1),
	RunE: revokeRun,
}

var updateCmd = &cobra.Command{
	Use:   "update SECRET [DATA]",
	Short: "Update an existing secret",
	Long: strings.Trim(`
Update an existing secret. If the secret does not exist, an error is returned.

Run with --create-if-missing to force creation of the secret if it does not
already exist.
`, "\n"),
	Example: strings.Trim(`
  # Update the secret named "api-key" with the contents "new-contents"
  berglas update my-secrets/api-key new-contents

  # Update the secret named "api-key" with a new KMS encryption key, keeping
  # the original secret value
  berglas update my-secrets/api-key --key=...

  # Update the secret named "api-key", creating it if it does not already exist
  berglas update my-secrets/api-key abcd1234 --create-if-missing --key...
`, "\n"),
	Args: cobra.RangeArgs(1, 2),
	RunE: updateRun,
}

var completionCmd = &cobra.Command{
	Use:   "completion SHELL",
	Args:  cobra.ExactArgs(1),
	Short: "Outputs shell completion for the given shell (bash or zsh)",
	Long: strings.Trim(
		`Outputs shell completion for the given shell (bash or zsh)

This depends on the bash-completion package. To install it:

  # Mac OS X
  brew install bash-completion

  # Debian
  apt-get install bash-completion

Zsh users may also put the file somewhere on their $fpath, like
/usr/local/share/zsh/site-functions
`, "\n"),
	Example: strings.Trim(`
  # Enable completion for bash users
  source <(berglas completion bash)

  # Enable completion for zsh users
  source <(berglas completion zsh)
`, "\n"),
	RunE: completionRun,
}

func main() {
	rootCmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	rootCmd.PersistentFlags().StringVarP(&logFormat, "log-format", "f", "console",
		"Format in which to log")
	rootCmd.PersistentFlags().StringVarP(&logLevel, "log-level", "l", "fatal",
		"Level at which to log")

	rootCmd.AddCommand(accessCmd)
	accessCmd.Flags().Int64Var(&accessGeneration, "generation", 0,
		"Get a specific generation")

	rootCmd.AddCommand(bootstrapCmd)
	bootstrapCmd.Flags().StringVar(&projectID, "project", "",
		"Google Cloud Project ID")
	if err := bootstrapCmd.MarkFlagRequired("project"); err != nil {
		panic(err)
	}
	bootstrapCmd.Flags().StringVar(&bucket, "bucket", "",
		"Name of the Cloud Storage bucket to create")
	if err := bootstrapCmd.MarkFlagRequired("bucket"); err != nil {
		panic(err)
	}
	bootstrapCmd.Flags().StringVar(&bucketLocation, "bucket-location", "US",
		"Location in which to create Cloud Storage bucket")
	bootstrapCmd.Flags().StringVar(&kmsLocation, "kms-location", "global",
		"Location in which to create the Cloud KMS key ring")
	bootstrapCmd.Flags().StringVar(&kmsKeyRing, "kms-keyring", "berglas",
		"Name of the KMS key ring to create")
	bootstrapCmd.Flags().StringVar(&kmsCryptoKey, "kms-key", "berglas-key",
		"Name of the KMS key to create")

	rootCmd.AddCommand(createCmd)
	createCmd.Flags().StringVar(&key, "key", "",
		"KMS key to use for encryption")
	if err := createCmd.MarkFlagRequired("key"); err != nil {
		panic(err)
	}

	rootCmd.AddCommand(deleteCmd)

	rootCmd.AddCommand(editCmd)
	editCmd.Flags().StringVar(&editor, "editor", "",
		"Editor program to use. If unspecified, this defaults to $VISUAL or "+
			"$EDITOR in that order.")
	editCmd.Flags().BoolVar(&createIfMissing, "create-if-missing", false,
		"Create the secret if it doesn't exist")
	editCmd.Flags().StringVar(&key, "key", "",
		"KMS key to use for encryption (only used when secret doesn't exist)")

	rootCmd.AddCommand(execCmd)
	execCmd.Flags().BoolVar(&execLocal, "local", false,
		"Parse local environment variables for secrets instead of querying the Cloud APIs")

	rootCmd.AddCommand(grantCmd)
	grantCmd.Flags().StringSliceVar(&members, "member", nil,
		"Member to add")

	rootCmd.AddCommand(listCmd)
	listCmd.Flags().BoolVar(&listGenerations, "all-generations", false,
		"List all versions of secrets")
	listCmd.Flags().StringVar(&listPrefix, "prefix", "",
		"List secrets that match prefix")

	rootCmd.AddCommand(revokeCmd)
	revokeCmd.Flags().StringSliceVar(&members, "member", nil,
		"Member to remove")

	rootCmd.AddCommand(updateCmd)
	updateCmd.Flags().BoolVar(&createIfMissing, "create-if-missing", false,
		"Create the secret if it does not already exist")
	updateCmd.Flags().StringVar(&key, "key", "",
		"KMS key to use for re-encryption")

	rootCmd.AddCommand(completionCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		if terr, ok := err.(*exitError); ok {
			os.Exit(terr.code)
		}
		os.Exit(1)
	}
}

func accessRun(_ *cobra.Command, args []string) error {
	client, ctx, closer, err := clientWithContext()
	if err != nil {
		return misuseError(err)
	}
	defer closer()

	bucket, object, err := parseRef(args[0])
	if err != nil {
		return misuseError(err)
	}

	plaintext, err := client.Access(ctx, &berglas.AccessRequest{
		Bucket:     bucket,
		Object:     object,
		Generation: accessGeneration,
	})
	if err != nil {
		return apiError(err)
	}

	fmt.Fprintf(stdout, "%s", plaintext)
	return nil
}

func bootstrapRun(_ *cobra.Command, args []string) error {
	client, ctx, closer, err := clientWithContext()
	if err != nil {
		return misuseError(err)
	}
	defer closer()

	if err := client.Bootstrap(ctx, &berglas.BootstrapRequest{
		ProjectID:      projectID,
		Bucket:         bucket,
		BucketLocation: bucketLocation,
		KMSLocation:    kmsLocation,
		KMSKeyRing:     kmsKeyRing,
		KMSCryptoKey:   kmsCryptoKey,
	}); err != nil {
		return apiError(err)
	}

	kmsKeyID := fmt.Sprintf("projects/%s/locations/%s/keyRings/%s/cryptoKeys/%s",
		projectID, kmsLocation, kmsKeyRing, kmsCryptoKey)

	fmt.Fprintf(stdout, "Successfully created berglas environment:\n")
	fmt.Fprintf(stdout, "\n")
	fmt.Fprintf(stdout, "  Bucket: %s\n", bucket)
	fmt.Fprintf(stdout, "  KMS key: %s\n", kmsKeyID)
	fmt.Fprintf(stdout, "\n")
	fmt.Fprintf(stdout, "To create a secret:\n")
	fmt.Fprintf(stdout, "\n")
	fmt.Fprintf(stdout, "  berglas create %s/my-secret abcd1234 \\\n", bucket)
	fmt.Fprintf(stdout, "    --key %s\n", kmsKeyID)
	fmt.Fprintf(stdout, "\n")
	fmt.Fprintf(stdout, "To grant access to that secret:\n")
	fmt.Fprintf(stdout, "\n")
	fmt.Fprintf(stdout, "  berglas grant %s/my-secret \\\n", bucket)
	fmt.Fprintf(stdout, "    --member user:jane.doe@mycompany.com\n")
	fmt.Fprintf(stdout, "\n")
	fmt.Fprintf(stdout, "For more help and examples, please run \"berglas -h\".\n")
	return nil
}

func createRun(_ *cobra.Command, args []string) error {
	client, ctx, closer, err := clientWithContext()
	if err != nil {
		return misuseError(err)
	}
	defer closer()

	bucket, object, err := parseRef(args[0])
	if err != nil {
		return misuseError(err)
	}

	data := strings.TrimSpace(args[1])
	plaintext, err := readData(data)
	if err != nil {
		return misuseError(err)
	}

	var secret *berglas.Secret
	if secret, err = client.Create(ctx, &berglas.CreateRequest{
		Bucket:    bucket,
		Object:    object,
		Key:       key,
		Plaintext: plaintext,
	}); err != nil {
		return apiError(err)
	}

	fmt.Fprintf(stdout, "Successfully created secret [%s] with generation [%d]\n", object, secret.Generation)
	return nil
}

func deleteRun(_ *cobra.Command, args []string) error {
	client, ctx, closer, err := clientWithContext()
	if err != nil {
		return misuseError(err)
	}
	defer closer()

	bucket, object, err := parseRef(args[0])
	if err != nil {
		return misuseError(err)
	}

	if err := client.Delete(ctx, &berglas.DeleteRequest{
		Bucket: bucket,
		Object: object,
	}); err != nil {
		return apiError(err)
	}

	fmt.Fprintf(stdout, "Successfully deleted secret [%s] if it existed\n", object)
	return nil
}

func editRun(_ *cobra.Command, args []string) error {
	client, ctx, closer, err := clientWithContext()
	if err != nil {
		return misuseError(err)
	}
	defer closer()

	// Find the editor
	var editor string
	for _, e := range []string{"VISUAL", "EDITOR"} {
		if v := os.Getenv(e); v != "" {
			editor = v
			break
		}
	}
	if editor == "" {
		err := errors.New("no editor is set - set VISUAL or EDITOR")
		return apiError(err)
	}

	bucket, object, err := parseRef(args[0])
	if err != nil {
		return misuseError(err)
	}

	// Get the existing secret
	originalSecret, err := client.Read(ctx, &berglas.ReadRequest{
		Bucket: bucket,
		Object: object,
	})
	if err != nil {
		return apiError(err)
	}

	// Create the tempfile
	f, err := ioutil.TempFile("", "berglas-")
	if err != nil {
		err = errors.Wrap(err, "failed to create tempfile for secret")
		return apiError(err)
	}

	defer func() {
		if err := os.Remove(f.Name()); err != nil {
			fmt.Fprintf(stderr, "failed to cleanup tempfile %s: %s\n", f.Name(), err)
		}
	}()

	// Write contents to the original file
	if _, err := f.Write(originalSecret.Plaintext); err != nil {
		err = errors.Wrap(err, "failed to write tempfile for secret")
		return apiError(err)
	}

	if err := f.Sync(); err != nil {
		err = errors.Wrap(err, "failed to sync tempfile for secret")
		return apiError(err)
	}

	if err := f.Close(); err != nil {
		err = errors.Wrap(err, "failed to close tempfile for secret")
		return apiError(err)
	}

	// Spawn editor
	editorSplit := strings.Split(editor, " ")
	editorCmd, editorArgs := editorSplit[0], editorSplit[1:]
	editorArgs = append(editorArgs, f.Name())
	cmd := exec.Command(editorCmd, editorArgs...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		err = errors.Wrap(err, "failed to start editor")
		return misuseError(err)
	}
	if err := cmd.Wait(); err != nil {
		if terr, ok := err.(*exec.ExitError); ok && terr.ProcessState != nil {
			code := terr.ProcessState.ExitCode()
			return exitWithCode(code, errors.Wrap(terr, "editor did not exit 0"))
		}
		err = errors.Wrap(err, "unknown failure in running editor")
		return misuseError(err)
	}

	// Read the new secret value
	newPlaintext, err := ioutil.ReadFile(f.Name())
	if err != nil {
		err = errors.Wrapf(err, "failed to read secret tempfile")
		return misuseError(err)
	}

	// Error if the secret is empty
	if len(newPlaintext) == 0 {
		err := errors.New("secret is empty")
		return misuseError(err)
	}

	if bytes.Equal(newPlaintext, originalSecret.Plaintext) {
		err := errors.New("secret unchanged - not going to update")
		return misuseError(err)
	}

	// Update the secret
	updatedSecret, err := client.Update(ctx, &berglas.UpdateRequest{
		Bucket:         bucket,
		Object:         object,
		Generation:     originalSecret.Generation,
		Key:            originalSecret.KMSKey,
		Metageneration: originalSecret.Metageneration,
		Plaintext:      newPlaintext,
	})
	if err != nil {
		err = errors.Wrapf(err, "failed to update secret")
		return misuseError(err)
	}

	fmt.Fprintf(stdout, "Successfully updated secret [%s] with generation [%d]\n",
		object, updatedSecret.Generation)
	return nil
}

func execRun(_ *cobra.Command, args []string) error {
	client, ctx, closer, err := clientWithContext()
	if err != nil {
		return misuseError(err)
	}
	defer closer()

	execCmd := args[0]
	execArgs := args[1:]

	c, err := berglas.New(ctx)
	if err != nil {
		return apiError(err)
	}

	env := os.Environ()

	if execLocal {
		// Parse local env
		for i, e := range env {
			p := strings.SplitN(e, "=", 2)
			if len(p) < 2 {
				continue
			}

			k, v := p[0], p[1]
			if !berglas.IsReference(v) {
				continue
			}

			s, err := c.Resolve(ctx, v)
			if err != nil {
				return apiError(err)
			}
			env[i] = fmt.Sprintf("%s=%s", k, s)
		}
	} else {
		// Parse remote env
		runtimeEnv, err := client.DetectRuntimeEnvironment()
		if err != nil {
			err = errors.Wrap(err, "failed to detect runtime environment")
			return misuseError(err)
		}

		envvars, err := runtimeEnv.EnvVars(ctx)
		if err != nil {
			err = errors.Wrap(err, "failed to find environment variables")
			return misuseError(err)
		}

		for k, v := range envvars {
			if !berglas.IsReference(v) {
				continue
			}

			s, err := c.Resolve(ctx, v)
			if err != nil {
				return apiError(err)
			}
			env = append(env, fmt.Sprintf("%s=%s", k, s))
		}
	}

	// Spawn the command
	cmd := exec.Command(execCmd, execArgs...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return misuseError(err)
	}

	// Listen for signals and send them to the underlying command
	doneCh := make(chan struct{})
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh)
	go func() {
		for {
			select {
			case s := <-signalCh:
				if cmd.Process == nil {
					return
				}
				if signalErr := cmd.Process.Signal(s); signalErr != nil && err == nil {
					fmt.Fprintf(stderr, "failed to signal command: %s\n", err)
				}
			case <-doneCh:
				close(signalCh)
				return
			}
		}
	}()

	// Wait for the command to finish
	if err := cmd.Wait(); err != nil {
		close(doneCh)
		if terr, ok := err.(*exec.ExitError); ok && terr.ProcessState != nil {
			code := terr.ProcessState.ExitCode()
			return exitWithCode(code, errors.Wrap(terr, "process exited non-zero"))
		}
		return misuseError(err)
	}
	return nil
}

func grantRun(_ *cobra.Command, args []string) error {
	client, ctx, closer, err := clientWithContext()
	if err != nil {
		return misuseError(err)
	}
	defer closer()

	bucket, object, err := parseRef(args[0])
	if err != nil {
		return misuseError(err)
	}

	sort.Strings(members)

	if err := client.Grant(ctx, &berglas.GrantRequest{
		Bucket:  bucket,
		Object:  object,
		Members: members,
	}); err != nil {
		return apiError(err)
	}

	fmt.Fprintf(stdout, "Successfully granted permission on [%s] to: \n- %s\n",
		object, strings.Join(members, "\n- "))
	return nil
}

func listRun(_ *cobra.Command, args []string) error {
	client, ctx, closer, err := clientWithContext()
	if err != nil {
		return misuseError(err)
	}
	defer closer()

	bucket := strings.TrimPrefix(args[0], "gs://")

	list, err := client.List(ctx, &berglas.ListRequest{
		Bucket:      bucket,
		Prefix:      listPrefix,
		Generations: listGenerations,
	})
	if err != nil {
		return apiError(err)
	}

	if len(list.Secrets) == 0 {
		return nil
	}

	tw := new(tabwriter.Writer)
	tw.Init(stdout, 0, 4, 4, ' ', 0)
	fmt.Fprintf(tw, "NAME\tGENERATION\tUPDATED\n")
	for _, s := range list.Secrets {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", s.Name, s.Generation, s.UpdatedAt.Local())
	}
	tw.Flush()

	return nil
}

func revokeRun(_ *cobra.Command, args []string) error {
	client, ctx, closer, err := clientWithContext()
	if err != nil {
		return misuseError(err)
	}
	defer closer()

	bucket, object, err := parseRef(args[0])
	if err != nil {
		return misuseError(err)
	}

	sort.Strings(members)

	if err := client.Revoke(ctx, &berglas.RevokeRequest{
		Bucket:  bucket,
		Object:  object,
		Members: members,
	}); err != nil {
		return apiError(err)
	}

	fmt.Fprintf(stdout, "Successfully revoked permission on [%s] from: \n- %s\n",
		object, strings.Join(members, "\n- "))
	return nil
}

func updateRun(_ *cobra.Command, args []string) error {
	client, ctx, closer, err := clientWithContext()
	if err != nil {
		return misuseError(err)
	}
	defer closer()

	bucket, object, err := parseRef(args[0])
	if err != nil {
		return misuseError(err)
	}

	var plaintext []byte
	if len(args) > 1 {
		plaintext, err = readData(strings.TrimSpace(args[1]))
		if err != nil {
			return misuseError(err)
		}
	}

	secret, err := client.Update(ctx, &berglas.UpdateRequest{
		Bucket:          bucket,
		Object:          object,
		Key:             key,
		Plaintext:       plaintext,
		CreateIfMissing: createIfMissing,
		Generation:      0,
		Metageneration:  0,
	})
	if err != nil {
		return apiError(err)
	}

	fmt.Fprintf(stdout, "Successfully updated secret [%s] to generation [%d]\n",
		object, secret.Generation)
	return nil
}

func completionRun(cmd *cobra.Command, args []string) error {
	switch shell := args[0]; shell {
	case "bash":
		if err := rootCmd.GenBashCompletion(stdout); err != nil {
			err = errors.Wrap(err, "failed to generate bash completion")
			return apiError(err)
		}
	case "zsh":
		if err := rootCmd.GenZshCompletion(stdout); err != nil {
			err = errors.Wrap(err, "failed to generate zsh completion")
			return apiError(err)
		}

		// enable the `source <(berglas completion SHELL)` pattern for zsh
		if _, err := io.WriteString(stdout, "compdef _berglas berglas\n"); err != nil {
			err = errors.Wrap(err, "failed to run compdef")
			return apiError(err)
		}
	default:
		err := errors.Errorf("unknown completion %q", shell)
		return misuseError(err)
	}

	return nil
}

// exitError is a typed error to return.
type exitError struct {
	err  error
	code int
}

// Error implements error.
func (e *exitError) Error() string {
	if e.err == nil {
		return "<missing error>"
	}
	return e.err.Error()
}

// exitWithCode prints exits with the specified error and exit code.
func exitWithCode(code int, err error) *exitError {
	return &exitError{
		err:  err,
		code: code,
	}
}

// apiError returns the given error with an API error exit code.
func apiError(err error) *exitError {
	return exitWithCode(APIExitCode, err)
}

// misuseError returns the given error with a userland exit code.
func misuseError(err error) *exitError {
	return exitWithCode(MisuseExitCode, err)
}

// logger returns the logger for this cli.
func logger() (*logrus.Logger, error) {
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse log level")
	}

	var formatter logrus.Formatter
	switch logFormat {
	case "console", "text":
		formatter = new(logrus.TextFormatter)
	case "json":
		formatter = new(berglas.LogFormatterStackdriver)
	default:
		return nil, errors.Errorf("unknown log format %q", logFormat)
	}

	return &logrus.Logger{
		Out:       stderr,
		Formatter: formatter,
		Hooks:     make(logrus.LevelHooks),
		Level:     level,
	}, nil
}

// clientWithContext returns an instantiated berglas client and context with a
// closer.
func clientWithContext() (*berglas.Client, context.Context, func(), error) {
	ctx, cancel := context.WithCancel(context.Background())

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()

	logger, err := logger()
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "failed to setup logger")
	}

	client, err := berglas.New(ctx)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "failed to create berglas client")
	}
	client.SetLogger(logger)

	return client, ctx, cancel, nil
}

// readData reads the given string. If the string starts with an "@", it is
// assumed to be a filepath. If the string starts with a "-", data is read from
// stdin. If the data starts with a "\", it is assumed to be an escape character
// only when specified as the first character.
func readData(s string) ([]byte, error) {
	switch {
	case strings.HasPrefix(s, "@"):
		return ioutil.ReadFile(s[1:])
	case strings.HasPrefix(s, "-"):
		r := bufio.NewReader(stdin)
		return r.ReadBytes('\n')
	case strings.HasPrefix(s, "\\"):
		return []byte(s[1:]), nil
	default:
		return []byte(s), nil
	}
}

// parseRef parses a secret ref into a bucket, secret path, and any errors.
func parseRef(s string) (string, string, error) {
	s = strings.TrimPrefix(s, "gs://")
	s = strings.TrimPrefix(s, "berglas://")

	ss := strings.SplitN(s, "/", 2)
	if len(ss) < 2 {
		return "", "", errors.Errorf("secret does not match format gs://<bucket>/<secret> or the format berglas://<bucket>/<secret>: %s", s)
	}

	return ss[0], ss[1], nil
}
