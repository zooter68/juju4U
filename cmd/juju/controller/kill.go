// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package controller

import (
	"fmt"
	"time"

	"github.com/juju/cmd"
	"github.com/juju/errors"
	"github.com/juju/gnuflag"
	"github.com/juju/utils/clock"

	"github.com/juju/juju/apiserver/common"
	"github.com/juju/juju/cmd/modelcmd"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/config"
)

const killDoc = `
Forcibly destroy the specified controller.  If the API server is accessible,
this command will attempt to destroy the controller model and all hosted models
and their resources.

If the API server is unreachable, the machines of the controller model will be
destroyed through the cloud provisioner.  If there are additional machines,
including machines within hosted models, these machines will not be destroyed
and will never be reconnected to the Juju controller being destroyed.

The normal process of killing the controller will involve watching the hosted
models as they are brought down in a controlled manner. If for some reason the
models do not stop cleanly, there is a default five minute timeout. If no change
in the model state occurs for the duration of this timeout, the command will
stop watching and destroy the models directly through the cloud provider.

See also:
    destroy-controller
    unregister
`

// NewKillCommand returns a command to kill a controller. Killing is a forceful
// destroy.
func NewKillCommand() cmd.Command {
	// Even though this command is all about killing a controller we end up
	// needing environment endpoints so we can fall back to the client destroy
	// environment method. This shouldn't really matter in practice as the
	// user trying to take down the controller will need to have access to the
	// controller environment anyway.
	return wrapKillCommand(&killCommand{
		clock: clock.WallClock,
	}, nil, clock.WallClock)
}

// wrapKillCommand provides the common wrapping used by tests and
// the default NewKillCommand above.
func wrapKillCommand(kill *killCommand, apiOpen modelcmd.APIOpener, clock clock.Clock) cmd.Command {
	if apiOpen == nil {
		apiOpen = modelcmd.OpenFunc(kill.JujuCommandBase.NewAPIRoot)
	}
	openStrategy := modelcmd.NewTimeoutOpener(apiOpen, clock, 10*time.Second)
	return modelcmd.WrapController(
		kill,
		modelcmd.WrapControllerSkipControllerFlags,
		modelcmd.WrapControllerSkipDefaultController,
		modelcmd.WrapControllerAPIOpener(openStrategy),
	)
}

// killCommand kills the specified controller.
type killCommand struct {
	destroyCommandBase

	clock   clock.Clock
	timeout time.Duration
}

// SetFlags implements Command.SetFlags.
func (c *killCommand) SetFlags(f *gnuflag.FlagSet) {
	c.destroyCommandBase.SetFlags(f)
	f.Var(newDurationValue(time.Minute*5, &c.timeout), "t", "Timeout before direct destruction")
	f.Var(newDurationValue(time.Minute*5, &c.timeout), "timeout", "")
}

// Info implements Command.Info.
func (c *killCommand) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "kill-controller",
		Args:    "<controller name>",
		Purpose: "Forcibly terminate all machines and other associated resources for a Juju controller.",
		Doc:     killDoc,
	}
}

// Init implements Command.Init.
func (c *killCommand) Init(args []string) error {
	return c.destroyCommandBase.Init(args)
}

// Run implements Command.Run
func (c *killCommand) Run(ctx *cmd.Context) error {
	controllerName := c.ControllerName()
	store := c.ClientStore()
	if !c.assumeYes {
		if err := confirmDestruction(ctx, controllerName); err != nil {
			return err
		}
	}

	// Attempt to connect to the API.
	api, err := c.getControllerAPI()
	switch {
	case err == nil:
		defer api.Close()
	case errors.Cause(err) == common.ErrPerm:
		return errors.Annotate(err, "cannot destroy controller")
	default:
		if errors.Cause(err) != modelcmd.ErrConnTimedOut {
			logger.Debugf("unable to open api: %s", err)
		}
		ctx.Infof("Unable to open API: %s\n", err)
		api = nil
	}

	// Obtain controller environ so we can clean up afterwards.
	controllerEnviron, err := c.getControllerEnviron(ctx, store, controllerName, api)
	if err != nil {
		return errors.Annotate(err, "getting controller environ")
	}
	// If we were unable to connect to the API, just destroy the controller through
	// the environs interface.
	if api == nil {
		ctx.Infof("Unable to connect to the API server, destroying through provider")
		return environs.Destroy(controllerName, controllerEnviron, store)
	}

	// Attempt to destroy the controller and all environments.
	err = api.DestroyController(true)
	if err != nil {
		ctx.Infof("Unable to destroy controller through the API: %s\nDestroying through provider", err)
		return environs.Destroy(controllerName, controllerEnviron, store)
	}

	ctx.Infof("Destroying controller %q\nWaiting for resources to be reclaimed", controllerName)

	uuid := controllerEnviron.Config().UUID()
	if err := c.WaitForModels(ctx, api, uuid); err != nil {
		c.DirectDestroyRemaining(ctx, api)
	}
	return environs.Destroy(controllerName, controllerEnviron, store)
}

// DirectDestroyRemaining will attempt to directly destroy any remaining
// models that have machines left.
func (c *killCommand) DirectDestroyRemaining(ctx *cmd.Context, api destroyControllerAPI) {
	hasErrors := false
	hostedConfig, err := api.HostedModelConfigs()
	if err != nil {
		hasErrors = true
		logger.Errorf("unable to retrieve hosted model config: %v", err)
	}
	for _, model := range hostedConfig {
		ctx.Infof("Killing %s/%s directly", model.Owner.Id(), model.Name)
		cfg, err := config.New(config.NoDefaults, model.Config)
		if err != nil {
			logger.Errorf(err.Error())
			hasErrors = true
			continue
		}
		env, err := environs.New(environs.OpenParams{
			Cloud:  model.CloudSpec,
			Config: cfg,
		})
		if err != nil {
			logger.Errorf(err.Error())
			hasErrors = true
			continue
		}
		if err := env.Destroy(); err != nil {
			logger.Errorf(err.Error())
			hasErrors = true
		} else {
			ctx.Infof("  done")
		}
	}
	if hasErrors {
		logger.Errorf("there were problems destroying some models, manual intervention may be necessary to ensure resources are released")
	} else {
		ctx.Infof("All hosted models destroyed, cleaning up controller machines")
	}
}

// WaitForModels will wait for the models to bring themselves down nicely.
// It will return the UUIDs of any models that need to be removed forceably.
func (c *killCommand) WaitForModels(ctx *cmd.Context, api destroyControllerAPI, uuid string) error {
	thirtySeconds := (time.Second * 30)
	updateStatus := newTimedStatusUpdater(ctx, api, uuid, c.clock)

	envStatus := updateStatus(0)
	lastStatus := envStatus.controller
	lastChange := c.clock.Now().Truncate(time.Second)
	deadline := lastChange.Add(c.timeout)
	// Check for both undead models and live machines, as machines may be
	// in the controller model.
	for ; hasUnreclaimedResources(envStatus) && (deadline.After(c.clock.Now())); envStatus = updateStatus(5 * time.Second) {
		now := c.clock.Now().Truncate(time.Second)
		if envStatus.controller != lastStatus {
			lastStatus = envStatus.controller
			lastChange = now
			deadline = lastChange.Add(c.timeout)
		}
		timeSinceLastChange := now.Sub(lastChange)
		timeUntilDestruction := deadline.Sub(now)
		warning := ""
		// We want to show the warning if it has been more than 30 seconds since
		// the last change, or we are within 30 seconds of our timeout.
		if timeSinceLastChange > thirtySeconds || timeUntilDestruction < thirtySeconds {
			warning = fmt.Sprintf(", will kill machines directly in %s", timeUntilDestruction)
		}
		ctx.Infof("%s%s", fmtCtrStatus(envStatus.controller), warning)
		for _, modelStatus := range envStatus.models {
			ctx.Verbosef(fmtModelStatus(modelStatus))
		}
	}
	if hasUnreclaimedResources(envStatus) {
		return errors.New("timed out")
	} else {
		ctx.Infof("All hosted models reclaimed, cleaning up controller machines")
	}
	return nil
}

type durationValue time.Duration

func newDurationValue(value time.Duration, p *time.Duration) *durationValue {
	*p = value
	return (*durationValue)(p)
}

func (d *durationValue) Set(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = durationValue(v)
	return err
}

func (d *durationValue) String() string { return (*time.Duration)(d).String() }
