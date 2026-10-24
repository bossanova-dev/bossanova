package home

import "github.com/recurser/boss/api/tui/ui"

func CheckoutRefusedByAccessState(err error) bool { return ui.CheckoutRefusedByAccessState(err) }
func CheckoutAlreadyActive(err error) bool        { return ui.CheckoutAlreadyActive(err) }

const CloudBillingUnavailableLine = ui.CloudBillingUnavailableLine

func CloudAccessUnavailableLine(err error) string { return ui.CloudAccessUnavailableLine(err) }
