/*
Copyright 2022 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	configv1 "github.com/openshift/api/config/v1"

	batchv1 "k8s.io/api/batch/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2/klogr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	azurecompute "github.com/Azure/azure-sdk-for-go/profiles/latest/compute/mgmt/compute"
	azureauth "github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	"github.com/openshift/cluster-control-plane-machine-set-operator/pkg/machineproviders/providers/openshift/machine/v1beta1/providerconfig"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

var (
	// errUnsupportedPlatform is returned when the platform is not supported.
	errUnsupportedPlatform = errors.New("unsupported platform")

	// errUnsupportedInstanceSize is returned when the instance size did not match the expected format.
	// Each platform will have it's own format for the instance size, and if we do not recognise the instance
	// size we cannot increase it.
	errInstanceTypeUnsupportedFormat = errors.New("instance type did not match expected format")

	// errUnsupportedInstanceSize is returned when the instance size is not supported.
	// This means that even though the format is correct, we haven't implemented the logic to increase
	// this instance size.
	errInstanceTypeNotSupported = errors.New("instance type is not supported")

	// errMissingInstanceSize is returned when the instance size is missing.
	errMissingInstanceSize = errors.New("instance size is missing")

	// errCredentialsSecret is returned when getting data from the cloud provider credentials secret fails or
	// constructing the secret fails.
	errCredentialsSecret = errors.New("credentials secret error")
)

// Framework is an interface for getting clients and information
// about the environment within test cases.
type Framework interface {
	// ControlPlaneMachineSetKey returns the object key for fetching a control plane
	// machine set.
	ControlPlaneMachineSetKey() runtimeclient.ObjectKey

	// LoadClient returns a new controller-runtime client.
	GetClient() runtimeclient.Client

	// GetContext returns a context.
	GetContext() context.Context

	// GetPlatformType returns the platform type.
	GetPlatformType() configv1.PlatformType

	// GetPlatformSupportLevel returns the support level for the current platform.
	GetPlatformSupportLevel() PlatformSupportLevel

	// GetScheme returns the scheme.
	GetScheme() *runtime.Scheme

	// NewEmptyControlPlaneMachineSet returns a new control plane machine set with
	// just the name and namespace set.
	NewEmptyControlPlaneMachineSet() *machinev1.ControlPlaneMachineSet

	// IncreaseProviderSpecInstanceSize increases the instance size of the
	// providerSpec passed. This is used to trigger updates to the Machines
	// managed by the control plane machine set.
	IncreaseProviderSpecInstanceSize(providerSpec *runtime.RawExtension) error

	// TagInstanceInProviderSpec tags the instance in the provider spec.
	TagInstanceInProviderSpec(providerSpec *runtime.RawExtension) error

	// ConvertToControlPlaneMachineSetProviderSpec converts a control plane machine provider spec
	// to a control plane machine set suitable provider spec.
	ConvertToControlPlaneMachineSetProviderSpec(providerSpec machinev1beta1.ProviderSpec) (*runtime.RawExtension, error)

	// UpdateDefaultedValueFromCPMS updates a field that is defaulted by the defaulting webhook in the MAO with a desired value.
	UpdateDefaultedValueFromCPMS(rawProviderSpec *runtime.RawExtension) (*runtime.RawExtension, error)

	// DeleteAnInstanceFromCloudProvider deletes an instance from the cloud provider.
	DeleteAnInstanceFromCloudProvider(machine *machinev1beta1.Machine) error

	// TerminateKubelet terminates kubelet on a node that is being referenced by the input machine.
	TerminateKubelet(node *corev1.Node, delObjects map[string]runtimeclient.Object) error
}

// PlatformSupportLevel is used to identify which tests should run
// based on the platform.
type PlatformSupportLevel int

const (
	// Unsupported means that the platform is not supported
	// by CPMS.
	Unsupported PlatformSupportLevel = iota
	// Manual means that the platform is supported by CPMS,
	// but the CPMS must be created manually.
	Manual
	// Full means that the platform is supported by CPMS,
	// and the CPMS will be created automatically.
	Full
)

const (
	// Namespace that contains the cloud credentials secret.
	namespaceSecret = "openshift-machine-api"

	// Name of the field that contains credentials within the cloud credentials secret for GCP.
	gcpCredentialsSecretKey = "service_account.json"
)

// framework is an implementation of the Framework interface.
// It is used to provide a common set of functionality to all of the
// test cases.
type framework struct {
	client       runtimeclient.Client
	logger       logr.Logger
	platform     configv1.PlatformType
	supportLevel PlatformSupportLevel
	scheme       *runtime.Scheme
	namespace    string
}

// NewFramework initialises a new test framework for the E2E suite.
func NewFramework() (Framework, error) {
	sch, err := loadScheme()
	if err != nil {
		return nil, err
	}

	client, err := loadClient(sch)
	if err != nil {
		return nil, err
	}

	supportLevel, platform, err := getPlatformSupportLevel(client)
	if err != nil {
		return nil, err
	}

	logger := klogr.New()
	ctrl.SetLogger(logger)

	return &framework{
		client:       client,
		logger:       logger,
		platform:     platform,
		supportLevel: supportLevel,
		scheme:       sch,
		namespace:    MachineAPINamespace,
	}, nil
}

// NewFrameworkWith initialises a new test framework for the E2E suite
// using the existing scheme, client, platform and support level provided.
func NewFrameworkWith(sch *runtime.Scheme, client runtimeclient.Client, platform configv1.PlatformType, supportLevel PlatformSupportLevel, namespace string) Framework {
	return &framework{
		client:       client,
		platform:     platform,
		supportLevel: supportLevel,
		scheme:       sch,
		namespace:    namespace,
	}
}

// ControlPlaneMachineSetKey is the object key for fetching a control plane
// machine set.
func (f *framework) ControlPlaneMachineSetKey() runtimeclient.ObjectKey {
	return runtimeclient.ObjectKey{
		Namespace: f.namespace,
		Name:      ControlPlaneMachineSetName,
	}
}

// GetClient returns a controller-runtime client.
func (f *framework) GetClient() runtimeclient.Client {
	return f.client
}

// GetContext returns a context.
func (f *framework) GetContext() context.Context {
	return context.Background()
}

// GetPlatformType returns the platform type.
func (f *framework) GetPlatformType() configv1.PlatformType {
	return f.platform
}

// GetPlatformSupportLevel returns the support level for the current platform.
func (f *framework) GetPlatformSupportLevel() PlatformSupportLevel {
	return f.supportLevel
}

// GetScheme returns the scheme.
func (f *framework) GetScheme() *runtime.Scheme {
	return f.scheme
}

// GetLogger returns the logger.
func (f *framework) GetLogger() logr.Logger {
	return f.logger
}

// NewEmptyControlPlaneMachineSet returns a new control plane machine set with
// just the name and namespace set.
func (f *framework) NewEmptyControlPlaneMachineSet() *machinev1.ControlPlaneMachineSet {
	return &machinev1.ControlPlaneMachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ControlPlaneMachineSetName,
			Namespace: f.namespace,
		},
	}
}

// IncreaseProviderSpecInstanceSize increases the instance size of the instance on the providerSpec
// that is passed.
func (f *framework) IncreaseProviderSpecInstanceSize(rawProviderSpec *runtime.RawExtension) error {
	providerConfig, err := providerconfig.NewProviderConfigFromMachineSpec(f.logger, machinev1beta1.MachineSpec{
		ProviderSpec: machinev1beta1.ProviderSpec{
			Value: rawProviderSpec,
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to get provider config: %w", err)
	}

	switch f.platform {
	case configv1.AWSPlatformType:
		return increaseAWSInstanceSize(rawProviderSpec, providerConfig)
	case configv1.AzurePlatformType:
		return increaseAzureInstanceSize(rawProviderSpec, providerConfig)
	case configv1.GCPPlatformType:
		return increaseGCPInstanceSize(rawProviderSpec, providerConfig)
	case configv1.NutanixPlatformType:
		return increaseNutanixInstanceSize(rawProviderSpec, providerConfig)
	case configv1.OpenStackPlatformType:
		return increaseOpenStackInstanceSize(rawProviderSpec, providerConfig)
	default:
		return fmt.Errorf("%w: %s", errUnsupportedPlatform, f.platform)
	}
}

// TagInstanceInProviderSpec tags the instance in the providerSpec.
func (f *framework) TagInstanceInProviderSpec(rawProviderSpec *runtime.RawExtension) error {
	providerConfig, err := providerconfig.NewProviderConfigFromMachineSpec(f.logger, machinev1beta1.MachineSpec{
		ProviderSpec: machinev1beta1.ProviderSpec{
			Value: rawProviderSpec,
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to get provider config: %w", err)
	}

	switch f.platform {
	case configv1.OpenStackPlatformType:
		return tagOpenStackProviderSpec(rawProviderSpec, providerConfig)
	default:
		return fmt.Errorf("%w: %s", errUnsupportedPlatform, f.platform)
	}
}

// UpdateDefaultedValueFromCPMS updates a defaulted value from the ControlPlaneMachineSet
// for either AWS, Azure or GCP.
func (f *framework) UpdateDefaultedValueFromCPMS(rawProviderSpec *runtime.RawExtension) (*runtime.RawExtension, error) {
	providerConfig, err := providerconfig.NewProviderConfigFromMachineSpec(f.logger, machinev1beta1.MachineSpec{
		ProviderSpec: machinev1beta1.ProviderSpec{
			Value: rawProviderSpec,
		},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get provider config: %w", err)
	}

	switch f.platform {
	case configv1.AzurePlatformType:
		return updateCredentialsSecretNameAzure(providerConfig)
	case configv1.AWSPlatformType:
		return updateCredentialsSecretNameAWS(providerConfig)
	case configv1.GCPPlatformType:
		return updateCredentialsSecretNameGCP(providerConfig)
	case configv1.NutanixPlatformType:
		return updateCredentialsSecretNameNutanix(providerConfig)
	default:
		return nil, fmt.Errorf("%w: %s", errUnsupportedPlatform, f.platform)
	}
}

// DeleteAnInstanceFromCloudProvider deletes an instances from a cloud provider.
// Currently supported are AWS, Azure and GCP.
func (f *framework) DeleteAnInstanceFromCloudProvider(machine *machinev1beta1.Machine) error {
	ctx := f.GetContext()
	client := f.client

	cpms := &machinev1.ControlPlaneMachineSet{}
	if err := client.Get(f.GetContext(), f.ControlPlaneMachineSetKey(), cpms); err != nil {
		return fmt.Errorf("control plane machine set should exist: %w", err)
	}

	switch f.GetPlatformType() {
	case configv1.AWSPlatformType:
		return deleteAWSInstance(ctx, client, machine)
	case configv1.AzurePlatformType:
		return deleteAzureInstance(ctx, client, machine)
	case configv1.GCPPlatformType:
		return deleteGCPInstance(ctx, client, f.logger, machine)
	}

	return nil
}

// TerminateKubelet terminates kubelet on a node that is being referenced by the input machine.
func (f *framework) TerminateKubelet(node *corev1.Node, delObjects map[string]runtimeclient.Object) error {
	client := f.GetClient()
	ctx := f.GetContext()

	serviceAccount, err := createServiceAccount(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to create a service account: %w", err)
	}

	delObjects[serviceAccount.Name] = serviceAccount

	role, err := createRole(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to create a role: %w", err)
	}

	delObjects[role.Name] = role

	roleBinding, err := createRoleBinding(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to create a role binding: %w", err)
	}

	delObjects[roleBinding.Name] = roleBinding

	job, err := createJob(ctx, client, node.Name, serviceAccount.Name)
	if err != nil {
		return fmt.Errorf("failed to create a job: %w", err)
	}

	delObjects[job.Name] = job

	return nil
}

func createServiceAccount(ctx context.Context, client runtimeclient.Client) (*corev1.ServiceAccount, error) {
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "termination-service-account",
			Namespace: MachineAPINamespace,
		},
	}

	if err := client.Create(ctx, serviceAccount); err != nil {
		return nil, fmt.Errorf("failed to create ServiceAccount: %w", err)
	}

	return serviceAccount, nil
}

func createRole(ctx context.Context, client runtimeclient.Client) (*rbacv1.Role, error) {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "termination-role",
			Namespace: MachineAPINamespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{"security.openshift.io"},
				ResourceNames: []string{"privileged"},
				Resources:     []string{"securitycontextconstraints"},
				Verbs:         []string{"use"},
			},
		},
	}

	if err := client.Create(ctx, role); err != nil {
		return nil, fmt.Errorf("failed to create Role: %w", err)
	}

	return role, nil
}

func createRoleBinding(ctx context.Context, client runtimeclient.Client) (*rbacv1.RoleBinding, error) {
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "termination-role-binding",
			Namespace: MachineAPINamespace,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "termination-role",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "termination-service-account",
				Namespace: MachineAPINamespace,
			},
		},
	}

	if err := client.Create(ctx, roleBinding); err != nil {
		return nil, fmt.Errorf("failed to create RoleBinding: %w", err)
	}

	return roleBinding, nil
}

func createJob(ctx context.Context, client runtimeclient.Client, nodeName, serviceAccountName string) (*batchv1.Job, error) { //nolint: funlen
	script := `apk update && apk add util-linux && chroot /host /bin/bash -c "systemctl stop kubelet";`
	hostPathDir := corev1.HostPathDirectory

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "termination-job",
			Namespace: MachineAPINamespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "termination-kubelet",
							Image:   "alpine:3.12",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{script},
							Env: []corev1.EnvVar{
								{
									Name:  "NAMESPACE",
									Value: MachineAPINamespace,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To[bool](true),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "host",
									MountPath: "/host",
								},
							},
						},
					},
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					NodeName:           nodeName,
					DNSPolicy:          corev1.DNSClusterFirstWithHostNet,
					ServiceAccountName: serviceAccountName,
					HostNetwork:        true,
					HostPID:            true,
					HostIPC:            true,
					Volumes: []corev1.Volume{
						{
							Name: "host",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/",
									Type: &hostPathDir,
								},
							},
						},
					},
				},
			},
		},
	}

	if err := client.Create(ctx, job); err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	return job, nil
}

// updateCredentialsSecretNameAzure updates the credentialSecret field from the ControlPlaneMachineSet.
func updateCredentialsSecretNameAzure(providerConfig providerconfig.ProviderConfig) (*runtime.RawExtension, error) {
	cfg := providerConfig.Azure().Config()
	cfg.CredentialsSecret = nil

	rawBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("error marshalling azure providerSpec: %w", err)
	}

	return &runtime.RawExtension{
		Raw: rawBytes,
	}, nil
}

// updateCredentialsSecretNameAWS updates the credentialSecret field from the ControlPlaneMachineSet.
func updateCredentialsSecretNameAWS(providerConfig providerconfig.ProviderConfig) (*runtime.RawExtension, error) {
	cfg := providerConfig.AWS().Config()
	cfg.CredentialsSecret = nil

	rawBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("error marshalling aws providerSpec: %w", err)
	}

	return &runtime.RawExtension{
		Raw: rawBytes,
	}, nil
}

// updateCredentialsSecretNameGCP updates the credentialSecret field from the ControlPlaneMachineSet.
func updateCredentialsSecretNameGCP(providerConfig providerconfig.ProviderConfig) (*runtime.RawExtension, error) {
	cfg := providerConfig.GCP().Config()
	cfg.CredentialsSecret = nil

	rawBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("error marshalling gcp providerSpec: %w", err)
	}

	return &runtime.RawExtension{
		Raw: rawBytes,
	}, nil
}

// updateCredentialsSecretNameNutanix updates the credentialSecret field from the ControlPlaneMachineSet.
func updateCredentialsSecretNameNutanix(providerConfig providerconfig.ProviderConfig) (*runtime.RawExtension, error) {
	cfg := providerConfig.Nutanix().Config()
	cfg.CredentialsSecret = nil

	rawBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("error marshalling nutanix providerSpec: %w", err)
	}

	return &runtime.RawExtension{
		Raw: rawBytes,
	}, nil
}

// ConvertToControlPlaneMachineSetProviderSpec converts a control plane machine provider spec
// to a raw, control plane machine set suitable provider spec.
func (f *framework) ConvertToControlPlaneMachineSetProviderSpec(providerSpec machinev1beta1.ProviderSpec) (*runtime.RawExtension, error) {
	providerConfig, err := providerconfig.NewProviderConfigFromMachineSpec(f.logger, machinev1beta1.MachineSpec{
		ProviderSpec: providerSpec,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get provider config: %w", err)
	}

	switch f.platform {
	case configv1.AWSPlatformType:
		return convertAWSProviderConfigToControlPlaneMachineSetProviderSpec(providerConfig)
	case configv1.AzurePlatformType:
		return convertAzureProviderConfigToControlPlaneMachineSetProviderSpec(providerConfig)
	case configv1.GCPPlatformType:
		return convertGCPProviderConfigToControlPlaneMachineSetProviderSpec(providerConfig)
	case configv1.NutanixPlatformType:
		return convertNutanixProviderConfigToControlPlaneMachineSetProviderSpec(providerConfig)
	case configv1.OpenStackPlatformType:
		return convertOpenStackProviderConfigToControlPlaneMachineSetProviderSpec(providerConfig)
	default:
		return nil, fmt.Errorf("%w: %s", errUnsupportedPlatform, f.platform)
	}
}

// convertAWSProviderConfigToControlPlaneMachineSetProviderSpec converts an AWS providerConfig into a
// raw control plane machine set provider spec.
func convertAWSProviderConfigToControlPlaneMachineSetProviderSpec(providerConfig providerconfig.ProviderConfig) (*runtime.RawExtension, error) {
	awsPs := providerConfig.AWS().Config()
	awsPs.Subnet = machinev1beta1.AWSResourceReference{}
	awsPs.Placement.AvailabilityZone = ""

	rawBytes, err := json.Marshal(awsPs)
	if err != nil {
		return nil, fmt.Errorf("error marshalling aws providerSpec: %w", err)
	}

	return &runtime.RawExtension{
		Raw: rawBytes,
	}, nil
}

// convertGCPProviderConfigToControlPlaneMachineSetProviderSpec converts a GCP providerConfig into a
// raw control plane machine set provider spec.
func convertGCPProviderConfigToControlPlaneMachineSetProviderSpec(providerConfig providerconfig.ProviderConfig) (*runtime.RawExtension, error) {
	gcpPs := providerConfig.GCP().Config()
	gcpPs.Zone = ""

	rawBytes, err := json.Marshal(gcpPs)
	if err != nil {
		return nil, fmt.Errorf("error marshalling gcp providerSpec: %w", err)
	}

	return &runtime.RawExtension{
		Raw: rawBytes,
	}, nil
}

// convertAzureProviderConfigToControlPlaneMachineSetProviderSpec converts an Azure providerConfig into a
// raw control plane machine set provider spec.
func convertAzureProviderConfigToControlPlaneMachineSetProviderSpec(providerConfig providerconfig.ProviderConfig) (*runtime.RawExtension, error) {
	azurePs := providerConfig.Azure().Config()
	azurePs.Zone = ""
	azurePs.Subnet = ""

	rawBytes, err := json.Marshal(azurePs)
	if err != nil {
		return nil, fmt.Errorf("error marshalling azure providerSpec: %w", err)
	}

	return &runtime.RawExtension{
		Raw: rawBytes,
	}, nil
}

// convertNutanixProviderConfigToControlPlaneMachineSetProviderSpec converts a Nutanix providerConfig into a
// raw control plane machine set provider spec.
func convertNutanixProviderConfigToControlPlaneMachineSetProviderSpec(providerConfig providerconfig.ProviderConfig) (*runtime.RawExtension, error) {
	nutanixProviderConfig := providerConfig.Nutanix().Config()

	rawBytes, err := json.Marshal(nutanixProviderConfig)
	if err != nil {
		return nil, fmt.Errorf("error marshalling nutanix providerSpec: %w", err)
	}

	return &runtime.RawExtension{
		Raw: rawBytes,
	}, nil
}

// convertOpenStackProviderConfigToControlPlaneMachineSetProviderSpec converts an OpenStack providerConfig into a
// raw control plane machine set provider spec.
func convertOpenStackProviderConfigToControlPlaneMachineSetProviderSpec(providerConfig providerconfig.ProviderConfig) (*runtime.RawExtension, error) {
	openStackPs := providerConfig.OpenStack().Config()

	openStackPs.AvailabilityZone = ""

	if openStackPs.RootVolume != nil {
		openStackPs.RootVolume.VolumeType = ""
		openStackPs.RootVolume.Zone = ""
	}

	rawBytes, err := json.Marshal(openStackPs)
	if err != nil {
		return nil, fmt.Errorf("error marshalling openstack providerSpec: %w", err)
	}

	return &runtime.RawExtension{
		Raw: rawBytes,
	}, nil
}

// loadClient returns a new controller-runtime client.
func loadClient(sch *runtime.Scheme) (runtimeclient.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get Kubernetes config: %w", err)
	}

	client, err := runtimeclient.New(cfg, runtimeclient.Options{
		Scheme: sch,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	return client, nil
}

// addToSchemeFunc is an alias for a function that will add types to the scheme.
// We use this to loop and handle the errors for each scheme.
type addToSchemeFunc func(*runtime.Scheme) error

// loadScheme creates a scheme with all of the required types for the
// tests, pre-registered.
func loadScheme() (*runtime.Scheme, error) {
	sch := scheme.Scheme

	var errs []error

	for _, f := range []addToSchemeFunc{
		configv1.AddToScheme,
		machinev1.AddToScheme,
		machinev1beta1.AddToScheme,
	} {
		if err := f(sch); err != nil {
			errs = append(errs, fmt.Errorf("failed to add to scheme: %w", err))
		}
	}

	if len(errs) > 0 {
		return nil, kerrors.NewAggregate(errs)
	}

	return sch, nil
}

// getPlatformSupportLevel returns the support level for the current platform.
func getPlatformSupportLevel(k8sClient runtimeclient.Client) (PlatformSupportLevel, configv1.PlatformType, error) {
	infra := &configv1.Infrastructure{}

	if err := k8sClient.Get(context.Background(), runtimeclient.ObjectKey{Name: "cluster"}, infra); err != nil {
		return Unsupported, configv1.NonePlatformType, fmt.Errorf("failed to get infrastructure resource: %w", err)
	}

	platformType := infra.Status.PlatformStatus.Type

	switch platformType {
	case configv1.AWSPlatformType:
		return Full, platformType, nil
	case configv1.AzurePlatformType:
		return Manual, platformType, nil
	case configv1.GCPPlatformType:
		return Manual, platformType, nil
	case configv1.NutanixPlatformType:
		return Manual, platformType, nil
	case configv1.OpenStackPlatformType:
		return Full, platformType, nil
	default:
		return Unsupported, platformType, nil
	}
}

// increaseAWSInstanceSize increases the instance size of the instance on the providerSpec for an AWS providerSpec.
func increaseAWSInstanceSize(rawProviderSpec *runtime.RawExtension, providerConfig providerconfig.ProviderConfig) error {
	cfg := providerConfig.AWS().Config()

	var err error

	cfg.InstanceType, err = nextAWSInstanceSize(cfg.InstanceType)
	if err != nil {
		return fmt.Errorf("failed to get next instance size: %w", err)
	}

	if err := setProviderSpecValue(rawProviderSpec, cfg); err != nil {
		return fmt.Errorf("failed to set provider spec value: %w", err)
	}

	return nil
}

// nextAWSInstanceSize returns the next AWS instance size in the series.
// In AWS terms this normally means doubling the size of the underlying instance.
// For example:
// - m6i.large -> m6i.xlarge
// - m6i.xlarge -> m6i.2xlarge
// - m6i.2xlarge -> m6i.4xlarge
// This should mean we do not need to update this when the installer changes the default instance size.
func nextAWSInstanceSize(current string) (string, error) {
	// Regex to match the AWS instance type string.
	re := regexp.MustCompile(`(?P<family>[a-z0-9]+)\.(?P<multiplier>\d)?(?P<size>[a-z]+)`)

	values := re.FindStringSubmatch(current)
	if len(values) != 4 {
		return "", fmt.Errorf("%w: %s", errInstanceTypeUnsupportedFormat, current)
	}

	family := values[1]
	size := values[3]

	if multiplier := values[2]; multiplier == "" {
		switch size {
		case "large":
			return fmt.Sprintf("%s.xlarge", family), nil
		case "xlarge":
			return fmt.Sprintf("%s.2xlarge", family), nil
		default:
			return "", fmt.Errorf("%w: %s", errInstanceTypeNotSupported, current)
		}
	}

	multiplierInt, err := strconv.Atoi(values[2])
	if err != nil {
		// This is a panic because the multiplier should always be a number.
		panic("failed to convert multiplier to int")
	}

	return fmt.Sprintf("%s.%d%s", family, multiplierInt*2, size), nil
}

// increaseAzureInstanceSize increases the instance size of the instance on the providerSpec for an Azure providerSpec.
func increaseAzureInstanceSize(rawProviderSpec *runtime.RawExtension, providerConfig providerconfig.ProviderConfig) error {
	cfg := providerConfig.Azure().Config()

	var err error

	cfg.VMSize, err = nextAzureVMSize(cfg.VMSize)
	if err != nil {
		return fmt.Errorf("failed to get next instance size: %w", err)
	}

	if err := setProviderSpecValue(rawProviderSpec, cfg); err != nil {
		return fmt.Errorf("failed to set provider spec value: %w", err)
	}

	return nil
}

// tagOpenStackProviderSpec adds a tag to the providerSpec for an OpenStack providerSpec.
func tagOpenStackProviderSpec(rawProviderSpec *runtime.RawExtension, providerConfig providerconfig.ProviderConfig) error {
	cfg := providerConfig.OpenStack().Config()

	randomTag := uuid.New().String()
	cfg.Tags = append(cfg.Tags, fmt.Sprintf("cpms-test-tag-%s", randomTag))

	if err := setProviderSpecValue(rawProviderSpec, cfg); err != nil {
		return fmt.Errorf("failed to set provider spec value: %w", err)
	}

	return nil
}

// nextAzureVMSize returns the next Azure VM size in the series.
// In Azure terms this normally means doubling the size of the underlying instance.
// This should mean we do not need to update this when the installer changes the default instance size.
func nextAzureVMSize(current string) (string, error) {
	// Regex to match the Azure VM size string.
	re := regexp.MustCompile(`Standard_(?P<family>[a-zA-Z]+)(?P<multiplier>[0-9]+)(?P<subfamily>[a-z]*)(?P<version>_v[0-9]+)?`)

	values := re.FindStringSubmatch(current)
	if len(values) != 5 {
		return "", fmt.Errorf("%w: %s", errInstanceTypeUnsupportedFormat, current)
	}

	family := values[1]
	subfamily := values[3]
	version := values[4]

	multiplier, err := strconv.Atoi(values[2])
	if err != nil {
		// This is a panic because the multiplier should always be a number.
		panic("failed to convert multiplier to int")
	}

	switch {
	case multiplier == 32:
		multiplier = 48
	case multiplier == 48:
		multiplier = 64
	case multiplier >= 64:
		return "", fmt.Errorf("%w: %s", errInstanceTypeNotSupported, current)
	default:
		multiplier *= 2
	}

	return fmt.Sprintf("Standard_%s%d%s%s", family, multiplier, subfamily, version), nil
}

// increaseGCPInstanceSize increases the instance size of the instance on the providerSpec for an GCP providerSpec.
func increaseGCPInstanceSize(rawProviderSpec *runtime.RawExtension, providerConfig providerconfig.ProviderConfig) error {
	cfg := providerConfig.GCP().Config()

	var err error

	cfg.MachineType, err = nextGCPMachineSize(cfg.MachineType)
	if err != nil {
		return fmt.Errorf("failed to get next instance size: %w", err)
	}

	if err := setProviderSpecValue(rawProviderSpec, cfg); err != nil {
		return fmt.Errorf("failed to set provider spec value: %w", err)
	}

	return nil
}

// increateNutanixInstanceSize increases the instance size of the instance on the providerSpec for an Nutanix providerSpec.
func increaseNutanixInstanceSize(rawProviderSpec *runtime.RawExtension, providerConfig providerconfig.ProviderConfig) error {
	cfg := providerConfig.Nutanix().Config()
	cfg.VCPUSockets++

	if err := setProviderSpecValue(rawProviderSpec, cfg); err != nil {
		return fmt.Errorf("failed to set provider spec value: %w", err)
	}

	return nil
}

// increase OpenStackInstanceSize increases the instance size of the instance on the providerSpec for an OpenStack providerSpec.
func increaseOpenStackInstanceSize(rawProviderSpec *runtime.RawExtension, providerConfig providerconfig.ProviderConfig) error {
	cfg := providerConfig.OpenStack().Config()

	if os.Getenv("OPENSTACK_CONTROLPLANE_FLAVOR_ALTERNATE") == "" {
		return fmt.Errorf("OPENSTACK_CONTROLPLANE_FLAVOR_ALTERNATE environment variable not set: %w", errMissingInstanceSize)
	} else {
		cfg.Flavor = os.Getenv("OPENSTACK_CONTROLPLANE_FLAVOR_ALTERNATE")
	}

	if err := setProviderSpecValue(rawProviderSpec, cfg); err != nil {
		return fmt.Errorf("failed to set provider spec value: %w", err)
	}

	return nil
}

// nextGCPVMSize returns the next GCP machine size in the series.
// The Machine sizes being used are in format <e2|n2|n1>-<subfamily(-subfamilyflavor(optional))>-<number>(-<number>(optional)).
//
//nolint:cyclop
func nextGCPMachineSize(current string) (string, error) {
	// Regex to match the GCP machine size string.
	re := regexp.MustCompile(`^(?P<family>[0-9a-z]+)-(?P<subfamily>[0-9a-z]+(-(?P<subfamilyflavor>[a-z]+))?)-(?P<multiplier>[0-9\.]+)(-(?P<multiplier2>[0-9]+))?`)

	subexpNames := re.SubexpNames()
	values := re.FindStringSubmatch(current)
	result := make(map[string]string)

	// The number of named regex subexpressions must always match the number of submatches.
	if len(values) != len(subexpNames) {
		return "", fmt.Errorf("%w: %s", errInstanceTypeUnsupportedFormat, current)
	}

	// Store the submatches into a subexpression name -> value map.
	for i, name := range subexpNames {
		if i != 0 && name != "" {
			result[name] = values[i]
		}
	}

	family, okFamily := result["family"]
	subfamily, okSubfamily := result["subfamily"]
	_, okMultiplier := result["multiplier"]
	subfamilyflavor, okSubfamilyflavor := result["subfamilyflavor"]

	if !(okFamily && okSubfamily && okMultiplier && okSubfamilyflavor) {
		return "", fmt.Errorf("%w: %s", errInstanceTypeUnsupportedFormat, current)
	}

	multiplier, err := strconv.ParseFloat(result["multiplier"], 64)
	if err != nil {
		// This is a panic because the multiplier should always be a number.
		panic("failed to convert multiplier to float")
	}

	var multiplier2 int

	if val, okMultiplier2 := result["multiplier2"]; okMultiplier2 && val != "" {
		var err error

		multiplier2, err = strconv.Atoi(val)
		if err != nil {
			// This is a panic because the multiplier2 should always be a number.
			panic("failed to convert multiplier2 to int")
		}
	}

	return setNextGCPMachineSize(current, family, subfamily, subfamilyflavor, multiplier, multiplier2)
}

// setNextGCPMachineSize returns the new GCP machine size in the series
// according to the family supported (e2, n1, n2).
//
//nolint:cyclop,funlen,gocognit,gocyclo
func setNextGCPMachineSize(current, family, subfamily, subfamilyflavor string, multiplier float64, multiplier2 int) (string, error) {
	switch {
	case strings.HasPrefix(subfamily, "custom"):
		ivCPU := int(multiplier)
		fvCPU := multiplier
		mem := multiplier2

		switch {
		case multiplier == 0 || mem == 0:
			return "", fmt.Errorf("%w: %s", errInstanceTypeNotSupported, current)

		case family == "n1":
			// You can create N1 custom machine types with 1 or more vCPUs.
			// Above 1 vCPU, you must increment the number of vCPUs by 2, up to 96 vCPUs for Intel Skylake platform,
			// or up to 64 vCPUs for Intel Broadwell, Haswell, or Ivy Bridge CPU platforms.
			// Note: cap it to 64 as we don't detect CPU.
			if ivCPU < 64 {
				ivCPU += 2
			}
			// For N1 machine types, select between 1 GB and 6.5 GB per vCPU, inclusive.
			// Note: use 3GB per vCPU, as that's a comfortable bump.
			mem = ivCPU * 3 * 1024

		case family == "n2":
			// For N2 custom machine types, you can create a machine type with 2 to 80 vCPUs and memory between 1 and 864 GB.
			// For machine types with up to 32 vCPUs, you can select a vCPU count that is a multiple of 2.
			// For machine types with greater than 32 vCPUs,
			// you must select a vCPU count that is a multiple of 4 (for example, 36, 40, 56, or 80).
			if ivCPU < 32 {
				ivCPU += 2
			} else if ivCPU <= 76 {
				ivCPU += 4
			}
			// For the N2 machine series, select between 0.5 GB and 8.0 GB per vCPU, inclusive.
			// Note: the max is 864GB.
			mem = ivCPU * 3 * 1024

		case family == "n2d":
			// You can create N2D custom machine types with 2, 4, 8, or 16 vCPUs.
			// After 16, you can increment the number of vCPUs by 16, up to 96 vCPUs.
			// The minimum acceptable number of vCPUs is 2.
			switch {
			case ivCPU == 2:
				ivCPU = 4
			case ivCPU == 4:
				ivCPU = 8
			case ivCPU == 8:
				ivCPU = 16
			case ivCPU == 96:
				// Keep unchanged.
			default:
				ivCPU += 16
			}
			// For N2D machine types, select between 0.5 GB and 8.0 GB per vCPU in 0.256 GB increments.
			mem = ivCPU * 3 * 1024

		case family == "e2" && subfamilyflavor == "micro":
			// 0.25 vCPU, 1 to 2 GB of memory.
			if mem >= (2 * 1024) {
				return "", fmt.Errorf("%w: %s", errInstanceTypeNotSupported, current)
			}

			mem += 1024

			return fmt.Sprintf("%s-%s-%.2f-%d", family, subfamily, fvCPU, mem), nil

		case family == "e2" && subfamilyflavor == "small":
			// 0.50 vCPU, 1 to 2 GB of memory.
			if mem >= (4 * 1024) {
				return "", fmt.Errorf("%w: %s", errInstanceTypeNotSupported, current)
			}

			mem += 1024

			return fmt.Sprintf("%s-%s-%.2f-%d", family, subfamily, fvCPU, mem), nil

		case family == "e2" && subfamilyflavor == "medium":
			// 1 vCPU, 1 to 2 GB of memory.
			if mem >= (8 * 1024) {
				return "", fmt.Errorf("%w: %s", errInstanceTypeNotSupported, current)
			}

			mem += 1024

			return fmt.Sprintf("%s-%s-%d-%d", family, subfamily, ivCPU, mem), nil

		case family == "e2" && subfamilyflavor == "":
			// You can create E2 custom machine types with vCPUs in multiples of 2, up to 32 vCPUs.
			// The minimum acceptable number of vCPUs for a VM is 2.
			if ivCPU < 32 {
				ivCPU += 2
			}
			// For E2, the ratio of memory per vCPU is 0.5 GB to 8 GB inclusive.
			mem = ivCPU * 3 * 1024
		}

		return fmt.Sprintf("%s-%s-%d-%d", family, subfamily, ivCPU, mem), nil

	case multiplier >= 32 && family == "e2":
		return "", fmt.Errorf("%w: %s", errInstanceTypeNotSupported, current)
	case multiplier == 32 && family == "n2":
		multiplier = 48
	case multiplier == 64 && family == "n2":
		multiplier = 80
	case multiplier == 64 || multiplier == 80:
		multiplier = 96
	case multiplier >= 96 && family == "n1":
		return "", fmt.Errorf("%w: %s", errInstanceTypeNotSupported, current)
	case multiplier == 96:
		multiplier = 128
	case multiplier >= 128:
		return "", fmt.Errorf("%w: %s", errInstanceTypeNotSupported, current)
	default:
		multiplier *= 2
	}

	return fmt.Sprintf("%s-standard-%d", family, int(multiplier)), nil
}

// setProviderSpecValue sets the value of the provider spec to the value that is passed.
func setProviderSpecValue(rawProviderSpec *runtime.RawExtension, value interface{}) error {
	providerSpecValue, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&value)
	if err != nil {
		return fmt.Errorf("failed to convert provider spec to unstructured: %w", err)
	}

	rawProviderSpec.Object = &unstructured.Unstructured{
		Object: providerSpecValue,
	}
	rawProviderSpec.Raw = nil

	return nil
}

// newConfigForStaticCreds gives us bytes that we should write to a temporary file with shared credentials for AWS.
func newConfigForStaticCreds(accessKey string, accessSecret string) []byte {
	buf := &bytes.Buffer{}
	fmt.Fprint(buf, "[default]\n")
	fmt.Fprintf(buf, "aws_access_key_id = %s\n", accessKey)
	fmt.Fprintf(buf, "aws_secret_access_key = %s\n", accessSecret)

	return buf.Bytes()
}

// sharedCredentialsFileFromSecret creates a new temporary file containing the AWS credentials.
func sharedCredentialsFileFromSecret(credentialsSecret *corev1.Secret) (string, error) {
	var data []byte

	if len(credentialsSecret.Data["aws_access_key_id"]) > 0 && len(credentialsSecret.Data["aws_secret_access_key"]) > 0 {
		data = newConfigForStaticCreds(
			string(credentialsSecret.Data["aws_access_key_id"]),
			string(credentialsSecret.Data["aws_secret_access_key"]),
		)
	} else {
		return "", fmt.Errorf("missing values in AWS credentials secret: %w", errCredentialsSecret)
	}

	f, err := os.CreateTemp("", "aws-shared-credentials")
	if err != nil {
		return "", fmt.Errorf("failed to create file for shared credentials: %w", err)
	}

	defer func() {
		if err := f.Close(); err != nil {
			panic(err)
		}
	}()

	if _, err := f.Write(data); err != nil {
		return "", fmt.Errorf("failed to write credentials to %s: %w", f.Name(), err)
	}

	return f.Name(), nil
}

// deleteAWSInstance deletes an instance from the AWS cloud provider.
func deleteAWSInstance(ctx context.Context, client runtimeclient.Client, machine *machinev1beta1.Machine) error {
	var credentialsSecret corev1.Secret
	if err := client.Get(ctx, runtimeclient.ObjectKey{
		Namespace: namespaceSecret,
		Name:      "aws-cloud-credentials",
	}, &credentialsSecret); err != nil {
		return fmt.Errorf("should be able to retrieve the cloud credentials secret for AWS: %w", err)
	}

	providerStatus := &machinev1beta1.AWSMachineProviderStatus{}
	providerStatusRaw := machine.Status.ProviderStatus

	if err := json.Unmarshal(providerStatusRaw.Raw, providerStatus); err != nil {
		return fmt.Errorf("failed to unmarshal provider status: %w", err)
	}

	instanceID := providerStatus.InstanceID
	region := machine.Labels["machine.openshift.io/region"]

	sessionOptions := session.Options{
		Config: aws.Config{
			Region: aws.String(region),
		},
	}

	sharedCredsFile, err := sharedCredentialsFileFromSecret(&credentialsSecret)
	if err != nil {
		return fmt.Errorf("could not create a shared credentials file from an AWS credentials secret: %w", errCredentialsSecret)
	}

	sessionOptions.SharedConfigState = session.SharedConfigEnable
	sessionOptions.SharedConfigFiles = []string{sharedCredsFile}

	session, err := session.NewSessionWithOptions(sessionOptions)
	if err != nil {
		return fmt.Errorf("could not create a session: %w", err)
	}

	awsClient := ec2.New(session)

	input := &ec2.TerminateInstancesInput{
		DryRun:      aws.Bool(false),
		InstanceIds: aws.StringSlice([]string{*instanceID}),
	}

	_, err = awsClient.TerminateInstances(input)
	if err != nil {
		return fmt.Errorf("should be able to delete an AWS instance: %w", err)
	}

	return nil
}

// azureCredentialsData contains values from the cloud credentials secret on Azure.
type azureCredentialsData struct {
	subscriptionID string
	resourceGroup  string
	clientID       string
	clientSecret   string
	tenantID       string
}

// getAzureCredentialsData gets values from cloud credentials secret.
func getAzureCredentialsData(credentialsSecret *corev1.Secret) (*azureCredentialsData, error) {
	subscriptionID, ok := credentialsSecret.Data["azure_subscription_id"]
	if !ok {
		return nil, fmt.Errorf("could not get subscriptionID from Azure credentials secret: %w", errCredentialsSecret)
	}

	resourceGroup, ok := credentialsSecret.Data["azure_resourcegroup"]
	if !ok {
		return nil, fmt.Errorf("could not get resourceGroup from Azure credentials secret: %w", errCredentialsSecret)
	}

	clientID, ok := credentialsSecret.Data["azure_client_id"]
	if !ok {
		return nil, fmt.Errorf("could not get clientID from Azure credentials secret: %w", errCredentialsSecret)
	}

	clientSecret, ok := credentialsSecret.Data["azure_client_secret"]
	if !ok {
		return nil, fmt.Errorf("could not get clientSecret from Azure credentials secret: %w", errCredentialsSecret)
	}

	tenantID, ok := credentialsSecret.Data["azure_tenant_id"]
	if !ok {
		return nil, fmt.Errorf("could not get tenantID from Azure credentials secret: %w", errCredentialsSecret)
	}

	return &azureCredentialsData{
		subscriptionID: string(subscriptionID),
		resourceGroup:  string(resourceGroup),
		clientID:       string(clientID),
		clientSecret:   string(clientSecret),
		tenantID:       string(tenantID),
	}, nil
}

// deleteAzureInstance deletes an instance from the Azure cloud provider.
func deleteAzureInstance(ctx context.Context, client runtimeclient.Client, machine *machinev1beta1.Machine) error {
	var credentialsSecret corev1.Secret
	if err := client.Get(ctx, runtimeclient.ObjectKey{
		Namespace: namespaceSecret,
		Name:      "azure-cloud-credentials",
	}, &credentialsSecret); err != nil {
		return fmt.Errorf("should be able to retrieve the cloud credentials secret for Azure: %w", err)
	}

	secretData, err := getAzureCredentialsData(&credentialsSecret)
	if err != nil {
		return fmt.Errorf("failed to retrieve data from credentials secret: %w", err)
	}

	vmName := machine.Status.NodeRef.Name

	authorizer, err := azureauth.NewClientCredentialsConfig(secretData.clientID, secretData.clientSecret, secretData.tenantID).Authorizer()
	if err != nil {
		return fmt.Errorf("failed to authenticate with Azure: %w", err)
	}

	vmClient := azurecompute.NewVirtualMachinesClient(secretData.subscriptionID)
	vmClient.Authorizer = authorizer

	forceDeletion := false

	_, err = vmClient.Delete(ctx, secretData.resourceGroup, vmName, &forceDeletion)
	if err != nil {
		return fmt.Errorf("should be able to delete an Azure instance: %w", err)
	}

	return nil
}

// deleteGCPInstance deletes an instance from the GCP cloud provider.
func deleteGCPInstance(ctx context.Context, client runtimeclient.Client, logger logr.Logger, machine *machinev1beta1.Machine) error {
	var credentialsSecret corev1.Secret
	if err := client.Get(ctx, runtimeclient.ObjectKey{
		Namespace: namespaceSecret,
		Name:      "gcp-cloud-credentials",
	}, &credentialsSecret); err != nil {
		return fmt.Errorf("should be able to retrieve the cloud credentials secret for GCP: %w", errCredentialsSecret)
	}

	serviceAccountJSON, ok := credentialsSecret.Data[gcpCredentialsSecretKey]
	if !ok {
		return fmt.Errorf("credentials secret does not have field %s set: %w", gcpCredentialsSecretKey, errCredentialsSecret)
	}

	creds, err := google.CredentialsFromJSON(ctx, serviceAccountJSON, compute.CloudPlatformScope)
	if err != nil {
		return fmt.Errorf("failed to get GCP credentials from JSON: %w", err)
	}

	computeService, err := compute.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return fmt.Errorf("failed to create new compute service: %w", err)
	}

	providerConfig, err := providerconfig.NewProviderConfigFromMachineSpec(logger, machinev1beta1.MachineSpec{
		ProviderSpec: machinev1beta1.ProviderSpec{
			Value: machine.Spec.ProviderSpec.Value,
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to get provider config: %w", err)
	}

	gcpProviderSpec := providerConfig.GCP().Config()

	projectID := gcpProviderSpec.ProjectID
	zone := gcpProviderSpec.Zone
	instanceName := machine.Status.NodeRef.Name

	_, err = computeService.Instances.Delete(projectID, zone, instanceName).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("should be able to delete a GCP instance: %w", err)
	}

	return nil
}
