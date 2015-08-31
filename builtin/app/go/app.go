package goapp

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hashicorp/otto/app"
	"github.com/hashicorp/otto/directory"
	"github.com/hashicorp/otto/helper/bindata"
	"github.com/hashicorp/otto/helper/packer"
	"github.com/hashicorp/otto/helper/terraform"
	"github.com/hashicorp/otto/helper/vagrant"
)

//go:generate go-bindata -pkg=goapp -nomemcopy ./data/...

// App is an implementation of app.App
type App struct{}

func (a *App) Compile(ctx *app.Context) (*app.CompileResult, error) {
	data := &bindata.Data{
		Asset:    Asset,
		AssetDir: AssetDir,
		Context: map[string]interface{}{
			"name":          ctx.Appfile.Application.Name,
			"dev_fragments": ctx.DevDepFragments,
			"path": map[string]string{
				"cache":    ctx.CacheDir,
				"compiled": ctx.Dir,
				"working":  filepath.Dir(ctx.Appfile.Path),
			},
		},
	}

	// Copy all the common files
	if err := data.CopyDir(ctx.Dir, "data/common"); err != nil {
		return nil, err
	}

	// Copy the infrastructure specific files
	prefix := fmt.Sprintf("data/%s-%s", ctx.Tuple.Infra, ctx.Tuple.InfraFlavor)
	if err := data.CopyDir(ctx.Dir, prefix); err != nil {
		return nil, err
	}

	return &app.CompileResult{
		DevDepFragmentPath: filepath.Join(ctx.Dir, "dev-dep/build/Vagrantfile.fragment"),
	}, nil
}

func (a *App) Build(ctx *app.Context) error {
	// Get the infrastructure state
	infra, err := ctx.Directory.GetInfra(&directory.Infra{
		Lookup: directory.Lookup{
			Infra: ctx.Appfile.ActiveInfrastructure().Name}})
	if err != nil {
		return err
	}

	if infra == nil || infra.State != directory.InfraStateReady {
		return fmt.Errorf(
			"Infrastructure for this application hasn't been built yet.\n" +
				"The build step requires this because the target infrastructure\n" +
				"as well as its final properties can affect the build process.\n" +
				"Please run `otto infra` to build the underlying infrastructure,\n" +
				"then run `otto build` again.")
	}

	// Construct the variables map for Packer
	variables := make(map[string]string)
	variables["aws_region"] = infra.Outputs["region"]
	variables["aws_access_key"] = ctx.InfraCreds["aws_access_key"]
	variables["aws_secret_key"] = ctx.InfraCreds["aws_secret_key"]

	// Start building the resulting build
	build := &directory.Build{
		App:         ctx.Tuple.App,
		Infra:       ctx.Tuple.Infra,
		InfraFlavor: ctx.Tuple.InfraFlavor,
		Artifact:    make(map[string]string),
	}

	// Build and execute Packer
	p := &packer.Packer{
		Dir:       ctx.Dir,
		Ui:        ctx.Ui,
		Variables: variables,
		Callbacks: map[string]packer.OutputCallback{
			"artifact": a.parseArtifact(build.Artifact),
		},
	}
	err = p.Execute("build", filepath.Join(ctx.Dir, "build", "template.json"))
	if err != nil {
		return err
	}

	// Store the build!
	if err := ctx.Directory.PutBuild(build); err != nil {
		return fmt.Errorf(
			"Error storing the build in the directory service: %s\n\n" +
				"Despite the build itself completing successfully, Otto must\n" +
				"also successfully store the results in the directory service\n" +
				"to be able to deploy this build. Please fix the above error and\n" +
				"rebuild.")
	}

	// Store the successful build
	ctx.Ui.Header("[green]Build success!")
	ctx.Ui.Message(
		"[green]The build was completed successfully and stored within\n" +
			"the directory service, meaning other members of your team\n" +
			"don't need to rebuild this same version and can deploy it\n" +
			"immediately.")

	return nil
}

func (a *App) Deploy(ctx *app.Context) error {
	// Get the infrastructure state
	infra, err := ctx.Directory.GetInfra(&directory.Infra{
		Lookup: directory.Lookup{
			Infra: ctx.Appfile.ActiveInfrastructure().Name}})
	if err != nil {
		return err
	}

	if infra == nil || infra.State != directory.InfraStateReady {
		return fmt.Errorf(
			"Infrastructure for this application hasn't been built yet.\n" +
				"The deploy step requires this because the target infrastructure\n" +
				"as well as its final properties can affect the deploy process.\n" +
				"Please run `otto infra` to build the underlying infrastructure,\n" +
				"then run `otto deploy` again.")
	}

	// Construct the variables map for Packer
	variables := make(map[string]string)
	variables["aws_region"] = infra.Outputs["region"]
	variables["aws_access_key"] = ctx.InfraCreds["aws_access_key"]
	variables["aws_secret_key"] = ctx.InfraCreds["aws_secret_key"]

	// Get the build information
	build, err := ctx.Directory.GetBuild(&directory.Build{
		App:         ctx.Tuple.App,
		Infra:       ctx.Tuple.Infra,
		InfraFlavor: ctx.Tuple.InfraFlavor,
	})
	if err != nil {
		return err
	}
	if build == nil {
		return fmt.Errorf(
			"This application hasn't been built yet. Please run `otto build`\n" +
				"first so that the deploy step has an artifact to deploy.")
	}

	// Get the AMI out of it
	ami, ok := build.Artifact[infra.Outputs["region"]]
	if !ok {
		return fmt.Errorf(
			"An artifact for the region '%s' could not be found. Please run\n"+
				"`otto build` and try again.",
			infra.Outputs["region"])
	}
	variables["ami"] = ami

	// Get our old deploy to populate the old state path if we have it
	deployLookup := &directory.Deploy{
		App:         ctx.Tuple.App,
		Infra:       ctx.Tuple.Infra,
		InfraFlavor: ctx.Tuple.InfraFlavor,
	}
	deploy, err := ctx.Directory.GetDeploy(&directory.Deploy{
		App:         ctx.Tuple.App,
		Infra:       ctx.Tuple.Infra,
		InfraFlavor: ctx.Tuple.InfraFlavor,
	})
	if err != nil {
		return err
	}
	if deploy == nil {
		// If we have no deploy, put in a temporary one
		deploy = deployLookup
		deploy.State = directory.DeployStateNew

		// Write the temporary deploy so we have an ID to use for the state
		if err := ctx.Directory.PutDeploy(deploy); err != nil {
			return err
		}
	}

	// Run Terraform!
	tf := &terraform.Terraform{
		Dir:       filepath.Join(ctx.Dir, "deploy"),
		Ui:        ctx.Ui,
		Variables: variables,
		Directory: ctx.Directory,
		StateId:   deploy.ID,
	}
	if err := tf.Execute("apply"); err != nil {
		return fmt.Errorf(
			"Error running Terraform: %s\n\n" +
				"Terraform usually has helpful error messages. Please read the error\n" +
				"messages above and resolve them. Sometimes simply running `otto deply`\n" +
				"again will work.")
	}

	return nil
}

func (a *App) parseArtifact(m map[string]string) packer.OutputCallback {
	return func(o *packer.Output) {
		// We're looking for ID events.
		//
		// Example: 1440649959,amazon-ebs,artifact,0,id,us-east-1:ami-9d66def6
		if len(o.Data) < 3 || o.Data[1] != "id" {
			return
		}

		// TODO: multiple AMIs
		parts := strings.Split(o.Data[2], ":")
		m[parts[0]] = parts[1]
	}
}

func (a *App) Dev(ctx *app.Context) error {
	return vagrant.Dev(ctx, &vagrant.DevOptions{
		Instructions: strings.TrimSpace(devInstructions),
	})
}

func (a *App) DevDep(dst, src *app.Context) (*app.DevDep, error) {
	// For Go, we build a binary using Vagrant, extract that binary,
	// and setup a Vagrantfile fragment to copy that binary in plus
	// setup the scripts to start it on boot.
	src.Ui.Header(fmt.Sprintf(
		"Building the dev dependency for '%s'", src.Appfile.Application.Name))
	src.Ui.Message(
		"To ensure cross-platform compatibility, we'll use Vagrant to\n" +
			"build this application. This is slow, and in a lot of cases we\n" +
			"can do something faster. Future versions of Otto will detect and\n" +
			"do this. As long as the application doesn't change, Otto will\n" +
			"cache the results of this build.\n\n")
	err := vagrant.Build(src, &vagrant.BuildOptions{
		Dir:    filepath.Join(src.Dir, "dev-dep/build"),
		Script: "/otto/build.sh",
	})
	if err != nil {
		return nil, err
	}

	// Return the fragment path we have setup
	return &app.DevDep{
		Files: []string{"dev-dep-output"},
	}, nil
}

const devInstructions = `
A development environment has been created for writing a generic Go-based
application. For this development environment, Go is pre-installed. To
work on your project, edit files locally on your own machine. The file changes
will be synced to the development environment.

When you're ready to build your project, run 'otto dev ssh' to enter
the development environment. You'll be placed directly into the working
directory where you can run 'go get' and 'go build' as you normally would.
The GOPATH is already completely setup.
`
