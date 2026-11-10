package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/heynemann/go-plg"
)

// secretPrefix starts the name of every environment variable holding a rule's secret.
const secretPrefix = "PRICING_SECRET_"

var errSecretUnset = errors.New("secret not set")

// envSecrets returns the service's plg.WithSecrets resolver: the secret <name> of the rule pricing/<rule> is the
// environment variable PRICING_SECRET_<RULE>_<NAME> (see secretVariable), so each rule reads only its own secrets,
// whatever names another rule's plugin.yaml declares. A variable unset or empty fails the rule's read; plg logs
// why (naming the variable, never a value) and tells the rule only that the secret could not be resolved.
func envSecrets(lookupEnv func(string) (string, bool)) func(context.Context, plg.PluginInfo, string) (string, error) {
	return func(_ context.Context, info plg.PluginInfo, name string) (string, error) {
		variable := secretVariable(info.Key, name)

		value, ok := lookupEnv(variable)
		if !ok || value == "" {
			return "", fmt.Errorf("%w: %s is empty or unset", errSecretUnset, variable)
		}

		return value, nil
	}
}

// secretVariable returns the environment variable of the secret name of the rule key ("pricing/<rule>"): the
// prefix, the rule and the name, upper-cased, every character but a letter or a digit becoming '_'. Thus the
// secret coupon_api_token of pricing/seasonal is PRICING_SECRET_SEASONAL_COUPON_API_TOKEN.
func secretVariable(key, name string) string {
	return secretPrefix + envName(strings.TrimPrefix(key, registration+"/")) + "_" + envName(name)
}

// envName upper-cases text, turning every character but an ASCII letter or digit into '_'.
func envName(text string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, text)
}
