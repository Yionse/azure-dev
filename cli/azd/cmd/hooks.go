package cmd

import (
	"context"
	"fmt"

	"github.com/azure/azure-dev/cli/azd/cmd/actions"
	"github.com/azure/azure-dev/cli/azd/internal"
	"github.com/azure/azure-dev/cli/azd/pkg/environment"
	"github.com/azure/azure-dev/cli/azd/pkg/exec"
	"github.com/azure/azure-dev/cli/azd/pkg/ext"
	"github.com/azure/azure-dev/cli/azd/pkg/input"
	"github.com/azure/azure-dev/cli/azd/pkg/output"
	"github.com/azure/azure-dev/cli/azd/pkg/output/ux"
	"github.com/azure/azure-dev/cli/azd/pkg/project"
	"github.com/azure/azure-dev/cli/azd/pkg/tools"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func hooksActions(root *actions.ActionDescriptor) *actions.ActionDescriptor {
	group := root.Add("hooks", &actions.ActionDescriptorOptions{
		Command: &cobra.Command{
			Use:   "hooks",
			Short: fmt.Sprintf("Develop, test and run hooks for an application. %s", output.WithWarningFormat("(Beta)")),
		},
		GroupingOptions: actions.CommandGroupOptions{
			RootLevelHelp: actions.CmdGroupConfig,
		},
	})

	group.Add("run", &actions.ActionDescriptorOptions{
		Command:        newHooksRunCmd(),
		FlagsResolver:  newHooksRunFlags,
		ActionResolver: newHooksRunAction,
	})

	return group
}

func newHooksRunFlags(cmd *cobra.Command, global *internal.GlobalCommandOptions) *hooksRunFlags {
	flags := &hooksRunFlags{}
	flags.Bind(cmd.Flags(), global)
	
	return flags
}

func newHooksRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <name>",
		Short: "Runs the specified hook for the project and services",
		Args:  cobra.ExactArgs(1),
	}
}

type hooksRunFlags struct {
	internal.EnvFlag
	global   *internal.GlobalCommandOptions
	platform string
	service  string
}

func (f *hooksRunFlags) Bind(local *pflag.FlagSet, global *internal.GlobalCommandOptions) {
	f.EnvFlag.Bind(local, global)
	f.global = global

	local.StringVar(&f.platform, "platform", "", "Forces hooks to run for the specified platform.")
	local.StringVar(&f.service, "service", "", "Only runs hooks for the specified service.")
}

type hooksRunAction struct {
	projectConfig *project.ProjectConfig
	env           *environment.Environment
	envManager    environment.Manager
	importManager *project.ImportManager
	commandRunner exec.CommandRunner
	console       input.Console
	flags         *hooksRunFlags
	args          []string
}

func newHooksRunAction(
	projectConfig *project.ProjectConfig,
	importManager *project.ImportManager,
	env *environment.Environment,
	envManager environment.Manager,
	commandRunner exec.CommandRunner,
	console input.Console,
	flags *hooksRunFlags,
	args []string,
) actions.Action {
	return &hooksRunAction{
		projectConfig: projectConfig,
		env:           env,
		envManager:    envManager,
		commandRunner: commandRunner,
		console:       console,
		flags:         flags,
		args:          args,
		importManager: importManager,
	}
}

const noHookFoundMessage = " (No hook found)"

// 1. 获取基础信息。检查hooks是否合法。区分ProjectLevel和ServiceLevel。然后processHooks。
func (hra *hooksRunAction) Run(ctx context.Context) (*actions.ActionResult, error) {
	hookName := hra.args[0]

	// Command title
	hra.console.MessageUxItem(ctx, &ux.MessageTitle{
		Title: "Running hooks (azd hooks run)",
		TitleNote: fmt.Sprintf(
			"Finding and executing %s hooks for environment %s",
			output.WithHighLightFormat(hookName),
			output.WithHighLightFormat(hra.env.Name()),
		),
	})

	// Validate service name
	if hra.flags.service != "" {
		if has, err := hra.importManager.HasService(ctx, hra.projectConfig, hra.flags.service); err != nil {
			return nil, err
		} else if !has {
			return nil, fmt.Errorf("service name '%s' doesn't exist", hra.flags.service)
		}
	}

	// Project level hooks
	projectHooks := hra.projectConfig.Hooks[hookName]

	if err := hra.processHooks(
		ctx,
		hra.projectConfig.Path,
		hookName,
		fmt.Sprintf("Running %d %s command hook(s) for project", len(projectHooks), hookName),
		fmt.Sprintf("Project: %s Hook Output", hookName),
		projectHooks,
		false,
	); err != nil {
		return nil, err
	}

	stableServices, err := hra.importManager.ServiceStable(ctx, hra.projectConfig)
	if err != nil {
		return nil, err
	}

	// Service level hooks
	for _, service := range stableServices {
		serviceHooks := service.Hooks[hookName]
		skip := hra.flags.service != "" && service.Name != hra.flags.service

		if err := hra.processHooks(
			ctx,
			service.RelativePath,
			hookName,
			fmt.Sprintf("Running %d %s service hook(s) for %s", len(serviceHooks), hookName, service.Name),
			fmt.Sprintf("%s: %s hook output", service.Name, hookName),
			serviceHooks,
			skip,
		); err != nil {
			return nil, err
		}
	}

	return &actions.ActionResult{
		Message: &actions.ResultMessage{
			Header: "Your hooks have been run successfully",
		},
	}, nil
}

// 2. 做更细致的检查
func (hra *hooksRunAction) processHooks(
	ctx context.Context,
	cwd string,
	hookName string,
	spinnerMessage string,
	previewMessage string,
	hooks []*ext.HookConfig,
	skip bool,
) error {
	hra.console.ShowSpinner(ctx, spinnerMessage, input.Step)

	// 为true跳过
	if skip {
		hra.console.StopSpinner(ctx, spinnerMessage, input.StepSkipped)
		return nil
	}

	// 查看是否有hooks需要执行
	if len(hooks) == 0 {
		hra.console.StopSpinner(ctx, spinnerMessage+noHookFoundMessage, input.StepWarning)
		return nil
	}

	// 检查是pre还是post
	hookType, commandName := ext.InferHookType(hookName)

	for _, hook := range hooks {
		// 检查配置项
		if err := hra.prepareHook(hookName, hook); err != nil {
			return err
		}

		// 循环执行
		err := hra.execHook(ctx, previewMessage, cwd, hookType, commandName, hook)
		if err != nil {
			hra.console.StopSpinner(ctx, spinnerMessage, input.StepFailed)
			return fmt.Errorf("failed running hook %s, %w", hookName, err)
		}

		// The previewer cancels the previous spinner so we need to restart/show it again.
		hra.console.StopSpinner(ctx, spinnerMessage, input.StepDone)
	}
	return nil
}

// 3. 注入运行时所需要的环境变量
func (hra *hooksRunAction) execHook(
	ctx context.Context,
	previewMessage string,
	cwd string,
	hookType ext.HookType,
	commandName string,
	hook *ext.HookConfig,
) error {
	hookName := string(hookType) + commandName

	hooksMap := map[string][]*ext.HookConfig{
		hookName: {hook},
	}

	hooksManager := ext.NewHooksManager(cwd)
	// hra.env为环境变量
	// &{test-tc-asdhkjh4 map[AZURE_ENV_NAME:test-tc-asdhkjh4] map[] 0xc000284ed0}
	hooksRunner := ext.NewHooksRunner(hooksManager, hra.commandRunner, hra.envManager, hra.console, cwd, hooksMap, hra.env)

	previewer := hra.console.ShowPreviewer(ctx, &input.ShowPreviewerOptions{
		Prefix:       "  ",
		Title:        previewMessage,
		MaxLineCount: 8,
	})
	defer hra.console.StopPreviewer(ctx, false)

	runOptions := &tools.ExecOptions{StdOut: previewer}
	// 再次执行
	// fmt.Println(hookType, '-', commandName, '-', hookName)
	// post  package  postpackage
	err := hooksRunner.RunHooks(ctx, hookType, runOptions, commandName)
	if err != nil {
		return err
	}

	return nil
}

// Overrides the configured hooks from command line flags
func (hra *hooksRunAction) prepareHook(name string, hook *ext.HookConfig) error {
	// Enable testing cross platform
	if hra.flags.platform != "" {
		platformType := ext.HookPlatformType(hra.flags.platform)
		switch platformType {
		case ext.HookPlatformWindows:
			if hook.Windows == nil {
				return fmt.Errorf("hook is not configured for Windows")
			} else {
				*hook = *hook.Windows
			}
		case ext.HookPlatformPosix:
			if hook.Posix == nil {
				return fmt.Errorf("hook is not configured for Posix")
			} else {
				*hook = *hook.Posix
			}
		default:
			return fmt.Errorf("platform %s is not valid. Supported values are windows & posix", hra.flags.platform)
		}
	}

	hook.Name = name
	hook.Interactive = false

	// Don't display the 'Executing hook...' messages
	hra.configureHookFlags(hook.Windows)
	hra.configureHookFlags(hook.Posix)

	return nil
}

func (hra *hooksRunAction) configureHookFlags(hook *ext.HookConfig) {
	if hook == nil {
		return
	}

	hook.Interactive = false
}
