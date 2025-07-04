// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package validator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"

	extensionswebhook "github.com/gardener/gardener/extensions/pkg/webhook"
	"github.com/gardener/gardener/pkg/apis/core"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gardener/gardener-extension-shoot-falco-service/pkg/apis/service"
	"github.com/gardener/gardener-extension-shoot-falco-service/pkg/constants"
	"github.com/gardener/gardener-extension-shoot-falco-service/pkg/profile"
)

// extra Falco options
type FalcoWebhookOptions struct {
	// if set to true, project namespace must be annotated with falco.gardener.cloud/enabled=true to
	// deploy Falco in their shoot clusters
	RestrictedUsage bool

	// if set to true, project namespace must be annotated with falco.gardener.cloud/centralized-logging=true
	// to use the Gardener manged centralized logging solution
	RestrictedCentralizedLogging bool
}

var DefautltFalcoWebhookOptions = FalcoWebhookOptions{}

// Complete implements Completer.Complete.
func (o *FalcoWebhookOptions) Complete() error {
	return nil
}

// Completed returns the completed Config. Only call this if `Complete` was successful.
func (c *FalcoWebhookOptions) Completed() *FalcoWebhookOptions {
	return c
}

// AddFlags implements Flagger.AddFlags.
func (c *FalcoWebhookOptions) AddFlags(fs *pflag.FlagSet) {
	fs.BoolVar(&c.RestrictedUsage, "restricted-usage", false, "if set to true, project namespaces must be annotated with falco.gardener.cloud/enabled=true to deploy Falco in their shoot clusters")
	fs.BoolVar(&c.RestrictedCentralizedLogging, "restricted-centralized-logging", false, "if set to true, project namespaces must be annotated with falco.gardener.cloud/centralized-logging=true to use the Gardener manged centralized logging solution")
}

// Apply sets the values of this Config in the given config.ControllerConfiguration.
func (c *FalcoWebhookOptions) Apply(config *FalcoWebhookOptions) {
	config.RestrictedCentralizedLogging = c.RestrictedCentralizedLogging
	config.RestrictedUsage = c.RestrictedUsage
}

// NewShootValidator returns a new instance of a shoot validator.
func NewShootValidator(mgr manager.Manager) extensionswebhook.Validator {
	return NewShootValidatorWithOption(mgr, &DefautltFalcoWebhookOptions)
}

func NewShootValidatorWithOption(mgr manager.Manager, options *FalcoWebhookOptions) extensionswebhook.Validator {

	restrictedUsage := options.RestrictedUsage
	// environment overwrites command line option
	if val, exists := os.LookupEnv("RESTRICTED_USAGE"); exists {
		if envOverwrite, err := strconv.ParseBool(val); err == nil {
			restrictedUsage = envOverwrite
		}
	}
	return &shoot{
		decoder:                  serializer.NewCodecFactory(mgr.GetScheme(), serializer.EnableStrict).UniversalDecoder(),
		restrictedUsage:          restrictedUsage,
		restrictedCentralLogging: options.RestrictedCentralizedLogging,
	}
}

// shoot validates shoots
type shoot struct {
	decoder                  runtime.Decoder
	restrictedUsage          bool
	restrictedCentralLogging bool
}

// Validate implements extensionswebhook.Validator.Validate
func (s *shoot) Validate(ctx context.Context, new, old client.Object) error {
	oldShoot, ok := old.(*core.Shoot)
	if !ok {
		oldShoot = nil
	}

	shoot, ok := new.(*core.Shoot)
	if !ok {
		return fmt.Errorf("wrong object type %T", new)
	}
	return s.validateShoot(ctx, shoot, oldShoot)
}

func (s *shoot) validateShoot(_ context.Context, shoot *core.Shoot, oldShoot *core.Shoot) error {
	// Need check here
	if s.isDisabled(shoot) {
		return nil
	}

	falcoConf, err := s.extractFalcoConfig(shoot)
	if err != nil {
		return err
	}

	oldFalcoConf, oldFalcoConfErr := s.extractFalcoConfig(oldShoot)

	if s.restrictedUsage {
		if oldFalcoConfErr != nil || oldFalcoConf == nil { // only verify elegibility if we can not read old shoot falco config or falco was not enabled before
			if ok := verifyNamespaceEligibility(shoot.Namespace); !ok {
				return fmt.Errorf("namespace %s is not eligible for Falco extension", shoot.Namespace)
			}
		}
	}

	if s.restrictedCentralLogging && isCentralLoggingEnabled(falcoConf) {
		if ok := verifyNamespaceEligibilityForCentralLogging(shoot.Namespace); !ok {
			return fmt.Errorf(
				"namespace %s is not eligible for centralized logging. Set destination to %s, %s or %s",
				shoot.Namespace,
				constants.FalcoEventDestinationStdout,
				constants.FalcoEventDestinationLogging,
				constants.FalcoEventDestinationCustom,
			)
		}
	}

	allErrs := []error{}

	if err := verifyFalcoVersion(falcoConf, oldFalcoConf); err != nil {
		allErrs = append(allErrs, err)
	}

	if err := verifyRules(falcoConf, shoot); err != nil {
		allErrs = append(allErrs, err)
	}

	if err := verifyEventDestinations(falcoConf, shoot); err != nil {
		allErrs = append(allErrs, err)
	}

	if len(allErrs) > 0 {
		return errors.Join(allErrs...)
	}

	return nil
}

func unique(slice []string) bool {
	unique := make(map[string]bool, len(slice))
	for _, elem := range slice {
		if _, ok := unique[elem]; ok {
			return false
		} else {
			unique[elem] = true
		}
	}
	return true
}

func verifyRules(falcoConf *service.FalcoServiceConfig, shoot *core.Shoot) error {
	if falcoConf.Rules == nil {
		return fmt.Errorf("rules property is not defined")
	}

	if falcoConf.Rules.StandardRules != nil {
		if standardRulesErr := verifyStandardRules(*falcoConf.Rules.StandardRules); standardRulesErr != nil {
			return standardRulesErr
		}
	}

	if falcoConf.Rules.CustomRules != nil {
		if customRulesErr := verifyCustomRules(*falcoConf.Rules.CustomRules, shoot); customRulesErr != nil {
			return customRulesErr
		}
	}

	numRules := 0
	if falcoConf.Rules.StandardRules != nil {
		numRules += len(*falcoConf.Rules.StandardRules)
	}
	if falcoConf.Rules.CustomRules != nil {
		numRules += len(*falcoConf.Rules.CustomRules)
	}
	if numRules == 0 {
		return fmt.Errorf("falco deployment without any rules is not allowed")
	}

	return nil
}

func verifyStandardRules(standardRules []string) error {
	for _, rule := range standardRules {
		if !slices.Contains(constants.AllowedStandardRules, rule) {
			return fmt.Errorf("unknown standard rule %s", rule)
		}
	}

	if !unique(standardRules) {
		return fmt.Errorf("duplicate entry in standard rules")
	}
	return nil
}

func verifyCustomRules(customRules []service.CustomRule, shoot *core.Shoot) error {
	customRulesNames := make([]string, 0)
	for _, rule := range customRules {
		if rule.ResourceName != "" && rule.ShootConfigMap != "" {
			return fmt.Errorf("found custom rule with both resource name and shoot config map defined")
		} else if rule.ResourceName == "" && rule.ShootConfigMap == "" {
			return fmt.Errorf("found custom rule with neither resource name nor shoot config map defined")
		}
		if rule.ResourceName != "" {
			customRulesNames = append(customRulesNames, rule.ResourceName)
		}
	}

	if !unique(customRulesNames) {
		return fmt.Errorf("duplicate entry in custom rules")
	}

	// note: we only verify rules defined in the shoot spec. we do not
	// verify custom rules stored in the shoot cluster
	// the reason is that the admission controller should not attempt to
	// contact the shoot cluster to verify the rules.
	for _, ruleName := range customRulesNames {
		found := false
		for _, r := range shoot.Spec.Resources {
			if r.ResourceRef.Kind == "ConfigMap" && r.Name == ruleName {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("custom rule %s not found in resources", ruleName)
		}
	}
	return nil
}

func verifyEventDestinations(falcoConf *service.FalcoServiceConfig, shoot *core.Shoot) error {
	if falcoConf.Destinations == nil {
		return fmt.Errorf("event destination property is not defined")
	}

	if len(*falcoConf.Destinations) == 0 {
		return fmt.Errorf("no event destination is set")
	}

	if len(*falcoConf.Destinations) > 2 {
		return fmt.Errorf("more than two event destinations are not allowed")
	}

	eventDestinationNames := make([]string, 0)
	for _, dest := range *falcoConf.Destinations {
		if !slices.Contains(constants.AllowedDestinations, dest.Name) {
			return fmt.Errorf("unknown event destination %s", dest.Name)
		}
		eventDestinationNames = append(eventDestinationNames, dest.Name)
	}

	if !unique(eventDestinationNames) {
		return fmt.Errorf("duplicate entry in event destinations")
	}

	if len(eventDestinationNames) > 1 {
		if !slices.Contains(eventDestinationNames, constants.FalcoEventDestinationStdout) {
			return fmt.Errorf("output destinations can only be paired with stdout")
		}
	}

	idxCustom := slices.IndexFunc(*falcoConf.Destinations, func(dest service.Destination) bool {
		return dest.Name == constants.FalcoEventDestinationCustom
	})

	if idxCustom != -1 { // custom event destination is set
		return verifyCustomDestination((*falcoConf.Destinations)[idxCustom], shoot)
	}

	return nil
}

func verifyCustomDestination(customDest service.Destination, shoot *core.Shoot) error {
	if customDest.ResourceSecretName == nil {
		return fmt.Errorf("custom event destination is set but no custom config is defined")
	}

	found := false
	for _, s := range shoot.Spec.Resources {
		if s.ResourceRef.Kind == "Secret" && s.Name == *customDest.ResourceSecretName {
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("custom event destination config %s not found in resources", *customDest.ResourceSecretName)
	}
	return nil
}

func verifyFalcoVersion(falcoConf *service.FalcoServiceConfig, oldFalcoConf *service.FalcoServiceConfig) error {
	if oldFalcoConf != nil && *oldFalcoConf.FalcoVersion == *falcoConf.FalcoVersion { // no version change
		return nil
	}

	versions := profile.FalcoProfileManagerInstance.GetFalcoVersions()
	if err := verifyFalcoVersionInVersions(falcoConf, versions); err != nil {
		return err
	}
	return nil
}

func verifyFalcoVersionInVersions(falcoConf *service.FalcoServiceConfig, versions *map[string]profile.FalcoVersion) error {
	chosenVersion := falcoConf.FalcoVersion
	if chosenVersion == nil {
		return fmt.Errorf("falcoVersion is nil")
	}

	for _, ver := range *versions {
		if *chosenVersion == ver.Version {
			if ver.Classification == "deprecated" &&
				ver.ExpirationDate != nil &&
				ver.ExpirationDate.Before(time.Now()) {
				return fmt.Errorf("chosen version is marked as deprecated")
			}
			return nil
		}
	}
	return fmt.Errorf("version not found in possible versions")
}

// isDisabled returns true if extension is explicitly disabled.
func (s *shoot) isDisabled(shoot *core.Shoot) bool {
	ext := s.findExtension(shoot)
	if ext == nil {
		return true
	}

	if ext.Disabled != nil {
		return *ext.Disabled
	}
	return false
}

// findExtension returns shoot-falco-service extension.
func (s *shoot) findExtension(shoot *core.Shoot) *core.Extension {
	for i, ext := range shoot.Spec.Extensions {
		if ext.Type == constants.ExtensionType {
			return &shoot.Spec.Extensions[i]
		}
	}
	return nil
}

func (s *shoot) extractFalcoConfig(shoot *core.Shoot) (*service.FalcoServiceConfig, error) {
	if shoot == nil {
		return nil, fmt.Errorf("shoot pointer was nil")
	}

	ext := s.findExtension(shoot)
	if ext != nil && ext.ProviderConfig != nil {
		falcoConfig := &service.FalcoServiceConfig{}
		if _, _, err := s.decoder.Decode(ext.ProviderConfig.Raw, nil, falcoConfig); err != nil {
			return nil, fmt.Errorf("failed to decode %s provider config: %w", ext.Type, err)
		}
		return falcoConfig, nil
	}
	return nil, fmt.Errorf("no FalcoConfig found in extensions")
}

func verifyNamespaceEligibility(namespace string) bool {
	always := slices.Contains(constants.AlwaysEnabledNamespaces[:], namespace)
	if always {
		return true
	}

	namespaceV1, ok := NamespacesInstance.namespaces[namespace]
	if !ok {
		return false
	}

	val, ok := namespaceV1.Annotations[constants.NamespaceEnableAnnotation]
	if !ok {
		return false
	}

	enabled, err := strconv.ParseBool(val)
	if err != nil {
		return false
	}
	return enabled
}

func verifyNamespaceEligibilityForCentralLogging(namespace string) bool {
	always := slices.Contains(constants.CentralLoggingAllowedNamespaces[:], namespace)
	if always {
		return true
	}

	namespaceV1, ok := NamespacesInstance.namespaces[namespace]
	if !ok {
		return false
	}

	val, ok := namespaceV1.Annotations[constants.NamespaceCentralLoggingAnnotation]
	if !ok {
		return false
	}

	enabled, err := strconv.ParseBool(val)
	if err != nil {
		return false
	}
	return enabled
}

func isCentralLoggingEnabled(falcoConf *service.FalcoServiceConfig) bool {
	if falcoConf.Destinations != nil && len(*falcoConf.Destinations) > 0 {
		for _, dest := range *falcoConf.Destinations {
			if dest.Name == constants.FalcoEventDestinationCentral {
				return true
			}
		}
	}
	return false
}
