package cloud

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/go-tfe"
	tfaddr "github.com/hashicorp/terraform-registry-address"
	"github.com/hashicorp/terraform-svchost/disco"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/backend"
	"github.com/hashicorp/terraform/internal/command/format"
	"github.com/hashicorp/terraform/internal/command/jsonformat"
	"github.com/hashicorp/terraform/internal/command/jsonprovider"
	"github.com/hashicorp/terraform/internal/command/views"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/terminal"
	"github.com/hashicorp/terraform/internal/terraform"
	"github.com/hashicorp/terraform/internal/tfdiags"
	tfversion "github.com/hashicorp/terraform/version"
)

// TestSuiteRunner executes any tests found in the relevant directories in TFC.
//
// It uploads the configuration and uses go-tfe to execute a .
//
// We keep this separate from Cloud, as the tests don't execute with a
// particular workspace in mind but instead with a specific module from a
// private registry. Many things within Cloud assume the existence of a
// workspace when initialising so it isn't pratical to share this for tests.
type TestSuiteRunner struct {

	// ConfigDirectory and TestingDirectory are the paths to the directory
	// that contains our configuration and our testing files.
	ConfigDirectory  string
	TestingDirectory string

	// Config is the actual loaded config.
	Config  *configs.Config
	Schemas *terraform.Schemas

	Services *disco.Disco

	// Source is the private registry module we should be sending the tests
	// to when they execute.
	Source string

	// GlobalVariables are the variables provided by the TF_VAR_* environment
	// variables and -var and -var-file flags.
	GlobalVariables map[string]backend.UnparsedVariableValue

	// Stopped and Cancelled track whether the user requested the testing
	// process to be interrupted. Stopped is a nice graceful exit, we'll still
	// tidy up any state that was created and mark the tests with relevant
	// `skipped` status updates. Cancelled is a hard stop right now exit, we
	// won't attempt to clean up any state left hanging, and tests will just
	// be left showing `pending` as the status. We will still print out the
	// destroy summary diagnostics that tell the user what state has been left
	// behind and needs manual clean up.
	Stopped   bool
	Cancelled bool

	// StoppedCtx and CancelledCtx allow in progress Terraform operations to
	// respond to external calls from the test command.
	StoppedCtx   context.Context
	CancelledCtx context.Context

	// Verbose tells the runner to print out plan files during each test run.
	Verbose bool

	// Filter restricts which test files will be executed.
	Filter []string

	// Renderer knows how to convert JSON logs retrieved from TFE back into
	// human-readable.
	//
	// If this is nil, the runner will print the raw logs directly to Streams.
	Renderer *jsonformat.Renderer

	// View and Streams provide alternate ways to output raw data to the
	// user.
	View    views.Test
	Streams *terminal.Streams

	// clientOverride allows tests to specify the client instead of letting the
	// system initialise one itself.
	clientOverride *tfe.Client
}

// Test runs the tests within directory in TFC/TFE.
func (runner *TestSuiteRunner) Test() tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	variables, variableDiags := ParseCloudRunTestVariables(runner.GlobalVariables)
	diags = diags.Append(variableDiags)
	if variableDiags.HasErrors() {
		// Stop early if we couldn't parse the global variables.
		return diags
	}

	addr, err := tfaddr.ParseModuleSource(runner.Source)
	if err != nil {
		if err, ok := err.(*tfaddr.ParserError); ok {
			diags = diags.Append(tfdiags.AttributeValue(
				tfdiags.Error,
				err.Summary,
				err.Detail,
				cty.Path{cty.GetAttrStep{Name: "source"}}))
		} else {
			diags = diags.Append(err)
		}
		return diags
	}

	if addr.Package.Host == tfaddr.DefaultModuleRegistryHost {
		// Then they've reference something from the public registry. We can't
		// run tests against that in this way yet.
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			"Module source points to the public registry",
			"Terraform Cloud can only execute tests for modules held within private registries.",
			cty.Path{cty.GetAttrStep{Name: "source"}}))
		return diags
	}

	id := tfe.RegistryModuleID{
		Organization: addr.Package.Namespace,
		Name:         addr.Package.Name,
		Provider:     addr.Package.TargetSystem,
		Namespace:    addr.Package.Namespace,
		RegistryName: tfe.PrivateRegistry,
	}

	client, module, clientDiags := runner.client(addr, id)
	diags = diags.Append(clientDiags)
	if clientDiags.HasErrors() {
		return diags
	}

	configurationVersion, err := client.TestRuns.CreateConfigurationVersion(runner.StoppedCtx, id)
	if err != nil {
		diags = diags.Append(generalError("Failed to create configuration version", err))
		return diags
	}

	if runner.Stopped || runner.Cancelled {
		return diags
	}

	if err := client.TestRuns.UploadConfigurationVersion(runner.StoppedCtx, configurationVersion.UploadURL, runner.ConfigDirectory); err != nil {
		diags = diags.Append(generalError("Failed to upload configuration version", err))
		return diags
	}

	if runner.Stopped || runner.Cancelled {
		return diags
	}

	opts := tfe.TestRunCreateOptions{
		Filter:        runner.Filter,
		TestDirectory: tfe.String(runner.TestingDirectory),
		Verbose:       tfe.Bool(runner.Verbose),
		Variables: func() []*tfe.RunVariable {
			runVariables := make([]*tfe.RunVariable, 0, len(variables))
			for name, value := range variables {
				runVariables = append(runVariables, &tfe.RunVariable{
					Key:   name,
					Value: value,
				})
			}
			return runVariables
		}(),
		ConfigurationVersion: configurationVersion,
		RegistryModule:       module,
	}

	run, err := client.TestRuns.Create(runner.StoppedCtx, opts)
	if err != nil {
		diags = diags.Append(generalError("Failed to create test run", err))
		return diags
	}

	var waitDiags tfdiags.Diagnostics
	run, waitDiags = runner.waitForRun(client, run)
	diags = diags.Append(waitDiags)
	if waitDiags.HasErrors() {
		return diags
	}

	logDiags := runner.renderLogs(client, run)
	diags = diags.Append(logDiags)
	return diags
}

func (runner *TestSuiteRunner) client(addr tfaddr.Module, id tfe.RegistryModuleID) (*tfe.Client, *tfe.RegistryModule, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	var client *tfe.Client
	if runner.clientOverride != nil {
		client = runner.clientOverride
	} else {
		service, err := discover(addr.Package.Host, runner.Services)
		if err != nil {
			diags = diags.Append(tfdiags.AttributeValue(
				tfdiags.Error,
				strings.ToUpper(err.Error()[:1])+err.Error()[1:],
				"", // no description is needed here, the error is clear
				cty.Path{cty.GetAttrStep{Name: "hostname"}},
			))
			return nil, nil, diags
		}

		// TODO: Possibly allow users to specify the token in the configuration.
		token, err := cliConfigToken(addr.Package.Host, runner.Services)
		if err != nil {
			diags = diags.Append(tfdiags.AttributeValue(
				tfdiags.Error,
				strings.ToUpper(err.Error()[:1])+err.Error()[1:],
				"", // no description is needed here, the error is clear
				cty.Path{cty.GetAttrStep{Name: "hostname"}},
			))
			return nil, nil, diags
		}

		if token == "" {
			hostname := addr.Package.Host.ForDisplay()

			loginCommand := "terraform login"
			if hostname != defaultHostname {
				loginCommand = loginCommand + " " + hostname
			}
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Required token could not be found",
				fmt.Sprintf(
					"Run the following command to generate a token for %s:\n    %s",
					hostname,
					loginCommand,
				),
			))
			return nil, nil, diags
		}

		cfg := &tfe.Config{
			Address:      service.String(),
			BasePath:     service.Path,
			Token:        token,
			Headers:      make(http.Header),
			RetryLogHook: runner.View.TFCRetryHook,
		}

		// Set the version header to the current version.
		cfg.Headers.Set(tfversion.Header, tfversion.Version)
		cfg.Headers.Set(headerSourceKey, headerSourceValue)

		if client, err = tfe.NewClient(cfg); err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Failed to create the Terraform Cloud/Enterprise client",
				fmt.Sprintf(
					`Encountered an unexpected error while creating the `+
						`Terraform Cloud/Enterprise client: %s.`, err,
				),
			))
			return nil, nil, diags
		}
	}

	module, err := client.RegistryModules.Read(runner.StoppedCtx, id)
	if err != nil {
		// Then the module doesn't exist, and we can't run tests against it.
		if err == tfe.ErrResourceNotFound {
			err = fmt.Errorf("module %q was not found.\n\nPlease ensure that the organization and hostname are correct and that your API token for %s is valid.", addr.ForDisplay(), addr.Package.Host.ForDisplay())
		}
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			fmt.Sprintf("Failed to read module %q", addr.ForDisplay()),
			fmt.Sprintf("Encountered an unexpected error while the module: %s", err),
			cty.Path{cty.GetAttrStep{Name: "source"}}))
		return client, nil, diags
	}

	// TODO: Validate if we can do this before the full release.
	// We know that before a certain version of TFE the test endpoints don't
	// exist, so we can check for that upfront. As of August 2023, we don't know
	// what version of TFE will contain the testing endpoints.

	/* TODO: Enable this when we're ready.
	currentAPIVersion, parseErr := version.NewVersion(client.RemoteAPIVersion())
	desiredAPIVersion, _ := version.NewVersion("2.7")

	if parseErr != nil || currentAPIVersion.LessThan(desiredAPIVersion) {
		log.Printf("[TRACE] API version check failed; want: >= %s, got: %s", desiredAPIVersion.Original(), currentAPIVersion)
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Unsupported Terraform Enterprise version",
			"The `terraform test` command is not supported with this version of Terraform Enterprise."))
		return client, module, diags
	}
	*/

	// Enable retries for server errors.
	client.RetryServerErrors(true)

	// Aaaaand I'm done.
	return client, module, diags
}

func (runner *TestSuiteRunner) waitForRun(client *tfe.Client, original *tfe.TestRun) (*tfe.TestRun, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	run := original
	started := time.Now()
	updated := started

	completed := func(i int) bool {
		var err error

		if run, err = client.TestRuns.Read(runner.StoppedCtx, run.ID); err != nil {
			diags = diags.Append(generalError("Failed to retrieve test run", err))
			return true
		}

		if run.Status != tfe.TestRunStatusQueued {
			// We block as long as the test run is still queued.
			return true
		}

		current := time.Now()
		if i == 0 || current.Sub(updated).Seconds() > 30 {
			updated = current

			// TODO: Provide better updates based on queue status etc.
			// We could look through the queue to find out exactly where the
			// test run is and give a count down. Other stuff like that.
			// For now, we'll just print a simple status updated.

			runner.View.TFCStatusUpdate(run.Status, current.Sub(started))
		}

		return false
	}

	handleCancelled := func(i int) {
		if err := client.TestRuns.ForceCancel(context.Background(), run.ID); err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Could not force cancel the test run",
				fmt.Sprintf("Terraform could not force cancel the test run, you will have to navigate to the Terraform Cloud console and cancel the test run manually.\n\nThe error message received when cancelling the test run was %s", err)))
			return
		}

		// Otherwise, we'll still wait for the operation to finish as we want to
		// render the logs later.
		//
		// At this point Terraform will just kill itself if the operation takes
		// too long to finish. We don't need to handle that here.

		for ; ; i++ {
			select {
			case <-time.After(backoff(backoffMin, backoffMax, i)):
				// Timer up, show status
			}

			if completed(i) {
				return
			}
		}

	}

	handleStopped := func(i int) {
		if err := client.TestRuns.Cancel(context.Background(), run.ID); err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Could not cancel the test run",
				fmt.Sprintf("Terraform could not cancel the test run, you will have to navigate to the Terraform Cloud console and cancel the test run manually.\n\nThe error message received when cancelling the test run was %s", err)))
			return
		}

		// We've requested a cancel, we'll just happily wait for the remote
		// operation to trigger everything and shut down nicely.

		for ; ; i++ {
			select {
			case <-runner.CancelledCtx.Done():
				handleCancelled(i)
				return
			case <-time.After(backoff(backoffMin, backoffMax, i)):
				// Timer up, show status
			}

			if completed(i) {
				return
			}
		}
	}

	for i := 0; ; i++ {
		select {
		case <-runner.StoppedCtx.Done():
			handleStopped(i)
			return run, diags
		case <-runner.CancelledCtx.Done():
			handleCancelled(i)
			return run, diags
		case <-time.After(backoff(backoffMin, backoffMax, i)):
			// Timer up, show status
		}

		if completed(i) {
			return run, diags
		}
	}
}

func (runner *TestSuiteRunner) renderLogs(client *tfe.Client, run *tfe.TestRun) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	logs, err := client.TestRuns.Logs(runner.StoppedCtx, run.ID)
	if err != nil {
		diags = diags.Append(generalError("Failed to retrieve logs", err))
		return diags
	}

	reader := bufio.NewReaderSize(logs, 64*1024)

	for next := true; next; {
		var l, line []byte
		var err error

		for isPrefix := true; isPrefix; {
			l, isPrefix, err = reader.ReadLine()
			if err != nil {
				if err != io.EOF {
					diags = diags.Append(generalError("Failed to read logs", err))
					return diags
				}
				next = false
			}

			line = append(line, l...)
		}

		if next || len(line) > 0 {

			if runner.Renderer != nil {
				log := jsonformat.JSONLog{}
				if err := json.Unmarshal(line, &log); err != nil {
					diags = diags.Append(generalError("Failed to render log line", err))
					runner.Streams.Println(string(line)) // Just print the raw line so the can still try and interpret the information.
					continue
				}

				// Most of the log types can be rendered with just the
				// information they contain. We just pass these straight into
				// the renderer. Others, however, need additional context that
				// isn't available within the renderer so we process them first.

				switch log.Type {
				case jsonformat.LogTestInterrupt:
					interrupt := log.TestFatalInterrupt

					runner.Streams.Eprintln(format.WordWrap(log.Message, runner.Streams.Stderr.Columns()))
					if len(interrupt.State) > 0 {
						runner.Streams.Eprint(format.WordWrap("\nTerraform has already created the following resources from the module under test:\n", runner.Streams.Stderr.Columns()))
						for _, resource := range interrupt.State {
							if len(resource.DeposedKey) > 0 {
								runner.Streams.Eprintf(" - %s (%s)\n", resource.Instance, resource.DeposedKey)
							} else {
								runner.Streams.Eprintf(" - %s\n", resource.Instance)
							}
						}
					}

					if len(interrupt.States) > 0 {
						for run, resources := range interrupt.States {
							runner.Streams.Eprint(format.WordWrap(fmt.Sprintf("\nTerraform has already created the following resources for %q:\n", run), runner.Streams.Stderr.Columns()))

							for _, resource := range resources {
								if len(resource.DeposedKey) > 0 {
									runner.Streams.Eprintf(" - %s (%s)\n", resource.Instance, resource.DeposedKey)
								} else {
									runner.Streams.Eprintf(" - %s\n", resource.Instance)
								}
							}
						}
					}

					if len(interrupt.Planned) > 0 {
						module := "the module under test"
						for _, run := range runner.Config.Module.Tests[log.TestFile].Runs {
							if run.Name == log.TestRun && run.ConfigUnderTest != nil {
								module = fmt.Sprintf("%q", run.Module.Source.String())
							}
						}

						runner.Streams.Eprint(format.WordWrap(fmt.Sprintf("\nTerraform was in the process of creating the following resources for %q from %s, and they may not have been destroyed:\n", log.TestRun, module), runner.Streams.Stderr.Columns()))
						for _, resource := range interrupt.Planned {
							runner.Streams.Eprintf("  - %s\n", resource)
						}
					}

				case jsonformat.LogTestPlan:
					var uimode plans.Mode
					for _, run := range runner.Config.Module.Tests[log.TestFile].Runs {
						if run.Name == log.TestRun {
							switch run.Options.Mode {
							case configs.RefreshOnlyTestMode:
								uimode = plans.RefreshOnlyMode
							case configs.NormalTestMode:
								uimode = plans.NormalMode
							}

							// Don't keep searching the runs.
							break
						}
					}

					plan := jsonformat.Plan{
						PlanFormatVersion:     log.TestPlan.FormatVersion,
						OutputChanges:         log.TestPlan.OutputChanges,
						ResourceChanges:       log.TestPlan.ResourceChanges,
						ResourceDrift:         log.TestPlan.ResourceDrift,
						RelevantAttributes:    log.TestPlan.RelevantAttributes,
						ProviderFormatVersion: jsonprovider.FormatVersion,
						ProviderSchemas:       jsonprovider.MarshalForRenderer(runner.Schemas),
					}
					runner.Renderer.RenderHumanPlan(plan, uimode)

				case jsonformat.LogTestState:
					state := jsonformat.State{
						StateFormatVersion:    log.TestState.FormatVersion,
						RootModule:            log.TestState.Values.RootModule,
						RootModuleOutputs:     log.TestState.Values.Outputs,
						ProviderFormatVersion: jsonprovider.FormatVersion,
						ProviderSchemas:       jsonprovider.MarshalForRenderer(runner.Schemas),
					}
					runner.Renderer.RenderHumanState(state)

				default:
					// For all the rest we can just hand over to the renderer
					// to handle directly.
					if err := runner.Renderer.RenderLog(&log); err != nil {
						diags = diags.Append(generalError("Failed to render log line", err))
						runner.Streams.Println(string(line)) // Just print the raw line so the can still try and interpret the information.
						continue
					}
				}

			} else {
				runner.Streams.Println(string(line)) // If the renderer is null, it means the user just wants to see the raw JSON outputs anyway.
			}
		}
	}

	return diags
}
