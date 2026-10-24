package domain

import "errors"

// ErrAmbiguousOrigin means multiple repositories share the canonical origin.
var ErrAmbiguousOrigin = errors.New("multiple repos match canonical origin URL")

// ErrTriggerLaunchClaimLost means an owner write no longer matches its claim.
var ErrTriggerLaunchClaimLost = errors.New("trigger launch claim lost")
