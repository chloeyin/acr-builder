// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Azure/acr-builder/builder"
	"github.com/Azure/acr-builder/cmder"
	"github.com/Azure/acr-builder/graph"
	"github.com/Azure/acr-builder/templating"
	"github.com/Azure/acr-builder/volume"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const execLongDesc = `
This command can be used to execute a pipeline.
`

type execCmd struct {
	out    io.Writer
	dryRun bool

	registry         string
	registryUserName string
	registryPassword string

	// Build-time parameters for rendering
	stepsFile        string
	valuesFile       string
	templateValues   []string
	buildID          string
	buildCommit      string
	buildTag         string
	buildRepository  string
	buildBranch      string
	buildTriggeredBy string
}

func newExecCmd(out io.Writer) *cobra.Command {
	e := &execCmd{
		out: out,
	}

	cmd := &cobra.Command{
		Use:   "exec",
		Short: "Execute a pipeline",
		Long:  execLongDesc,
		RunE:  e.run,
	}

	f := cmd.Flags()

	f.StringVar(&e.valuesFile, "values", "", "the values file to use")
	f.StringVar(&e.stepsFile, "steps", "", "the steps file to use")
	f.StringArrayVar(&e.templateValues, "set", []string{}, "set values on the command line (use `--set` multiple times or use commas: key1=val1,key2=val2)")

	f.StringVarP(&e.registry, "registry", "r", "", "the name of the registry")
	f.StringVarP(&e.registryUserName, "username", "u", "", "the username to use when logging into the registry")
	f.StringVarP(&e.registryPassword, "password", "p", "", "the password to use when logging into the registry")

	// TODO: better names and shorthand
	// Rendering parameters
	f.StringVar(&e.buildID, "id", "", "the build ID")
	f.StringVarP(&e.buildCommit, "commit", "c", "", "the commit SHA")
	f.StringVarP(&e.buildTag, "tag", "t", "", "the build tag")
	f.StringVar(&e.buildRepository, "repository", "", "the build repository")
	f.StringVarP(&e.buildBranch, "branch", "b", "", "the build branch")
	f.StringVar(&e.buildTriggeredBy, "triggered-by", "", "what the build was triggered by")

	f.BoolVar(&e.dryRun, "dry-run", false, "evaluates the pipeline but doesn't execute it")

	return cmd
}

func (e *execCmd) run(cmd *cobra.Command, args []string) error {
	cmder := cmder.NewCmder(e.dryRun)

	ctx := context.Background()

	template, err := templating.LoadTemplate(e.stepsFile)
	if err != nil {
		return fmt.Errorf("failed to load template at path %s. Err: %v", e.stepsFile, err)
	}
	config, err := templating.LoadConfig(e.valuesFile)
	if err != nil {
		return fmt.Errorf("failed to load values file at path %s. Err: %v", e.valuesFile, err)
	}

	engine := templating.New()

	bo := templating.BaseRenderOptions{
		ID:          e.buildID,
		Commit:      e.buildCommit,
		Tag:         e.buildTag,
		Repository:  e.buildRepository,
		Branch:      e.buildBranch,
		TriggeredBy: e.buildTriggeredBy,
		Registry:    e.registry,
	}

	rawVals, err := combineVals(e.templateValues)
	if err != nil {
		return err
	}

	setConfig := &templating.Config{RawValue: rawVals, Values: map[string]*templating.Value{}}
	vals, err := templating.OverrideValuesWithBuildInfo(config, setConfig, bo)
	if err != nil {
		return fmt.Errorf("Failed to override values: %v", err)
	}

	rendered, err := engine.Render(template, vals)
	if err != nil {
		return fmt.Errorf("Error while rendering templates: %v", err)
	}

	if rendered[e.stepsFile] == "" {
		return errors.New("rendered template was empty")
	}

	if debug {
		fmt.Println("Rendered template:")
		fmt.Println(rendered[e.stepsFile])
	}

	p, err := graph.UnmarshalPipelineFromString(rendered[e.stepsFile])
	if err != nil {
		return err
	}

	dag, err := graph.NewDagFromPipeline(p)
	if err != nil {
		return err
	}

	timeout := time.Duration(p.TotalTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	homeVolName := fmt.Sprintf("%s%s", volume.VolumePrefix, uuid.New())
	if !e.dryRun {
		fmt.Printf("Setting up the home volume: %s\n", homeVolName)
		v := volume.NewVolume(homeVolName, cmder)
		if msg, err := v.Create(ctx); err != nil {
			return fmt.Errorf("Err creating docker vol. Msg: %s, Err: %v", msg, err)
		}
		defer func() {
			if msg, err := v.Delete(ctx); err != nil {
				fmt.Printf("Failed to clean up docker vol: %s. Msg: %s, Err: %v\n", homeVolName, msg, err)
			}
		}()
	}

	buildOptions := &builder.BuildOptions{
		RegistryName:     e.registry,
		RegistryUsername: e.registryUserName,
		RegistryPassword: e.registryPassword,
		Push:             len(p.Push) > 0,
	}

	builder := builder.NewBuilder(cmder, debug, homeVolName, buildOptions)
	defer builder.CleanAllBuildSteps(context.Background(), dag)

	return builder.RunAllBuildSteps(ctx, dag, p.Push)
}

func combineVals(values []string) (string, error) {
	ret := templating.Values{}
	for _, v := range values {
		s := strings.Split(v, "=")
		if len(s) != 2 {
			return "", fmt.Errorf("failed to parse --set data: %s", v)
		}
		ret[s[0]] = s[1]
	}

	return ret.ToTOMLString()
}