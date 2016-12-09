package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"code.cloudfoundry.org/lager"

	"github.com/fatih/structs"
	uuid "github.com/hashicorp/go-uuid"
	"github.com/hashicorp/vault/api"
	"github.com/mitchellh/mapstructure"
	"github.com/pivotal-cf/brokerapi"
)

const (
	// VaultBrokerName is the name we use for the broker
	VaultBrokerName = "vault"

	// VaultBrokerDescription is the description we use for the broker
	VaultBrokerDescription = "HashiCorp Vault Service Broker"

	// VaultPlanName is the name of our plan, only one supported
	VaultPlanName = "default"

	// VaultPlanDescription is the description of the plan
	VaultPlanDescription = "Secure access to a multi-tenant HashiCorp Vault cluster"

	// VaultPeriodicTTL is the token role periodic TTL.
	VaultPeriodicTTL = 5 * 86400
)

var _ brokerapi.ServiceBroker = (*Broker)(nil)

type bindingInfo struct {
	Binding       string
	ClientToken   string
	Accessor      string
	LeaseDuration int
	Renew         time.Time
	Expires       time.Time

	timer *time.Timer
}

type Broker struct {
	log    lager.Logger
	client *api.Client

	// mountMutex is used to protect updates to the mount table
	mountMutex sync.Mutex

	// Binds is used to track all the bindings and perform
	// their renewal at (Expiration/2) intervals.
	binds    map[string]*bindingInfo
	bindLock sync.Mutex

	running bool
	runLock sync.Mutex
}

// Start is used to start the broker
func (b *Broker) Start() error {
	b.runLock.Lock()
	defer b.runLock.Unlock()

	// Do nothing if started
	if b.running {
		return nil
	}

	// Ensure binds is initialized
	b.binds = make(map[string]*bindingInfo)

	// Ensure the generic secret backend at cf/broker is mounted.
	mounts := map[string]string{
		"cf/broker": "generic",
	}
	if err := b.IdempotentMount(mounts); err != nil {
		b.log.Error("broker: failed creating mounts", err)
		return fmt.Errorf("failed to create broker state mount: %v", err)
	}

	// Restore timers
	b.log.Info("broker: starting restore of binds")
	instances, err := b.listDir("cf/broker/")
	if err != nil {
		b.log.Error("broker: failed to list instances", err)
		return fmt.Errorf("failed to list instances")
	}
	for _, inst := range instances {
		binds, err := b.listDir("cf/broker/" + inst + "/")
		if err != nil {
			b.log.Error("broker: failed to list binds", err)
			return fmt.Errorf("failed to list binds")
		}
		for _, bind := range binds {
			if err := b.restoreBind(inst, bind); err != nil {
				b.log.Error("broker: failed to restore bind", err)
				return fmt.Errorf("failed to restore bind")
			}
		}
	}

	// Log our restore status
	b.bindLock.Lock()
	b.log.Info(fmt.Sprintf("broker: restored %d binds from %d instances", len(b.binds), len(instances)))
	b.bindLock.Unlock()

	b.running = true
	return nil
}

// listDir is used to list a directory
func (b *Broker) listDir(dir string) ([]string, error) {
	secret, err := b.client.Logical().List("cf/broker/")
	if err != nil {
		return nil, err
	}
	if secret != nil && len(secret.Data) > 0 {
		keysRaw := secret.Data["keys"].([]string)
		return keysRaw, nil
	}
	return nil, nil
}

// restoreBind is used to restore a binding
func (b *Broker) restoreBind(instanceID, bindingID string) error {
	// Read from Vault
	path := "cf/broker/" + instanceID + "/" + bindingID
	secret, err := b.client.Logical().Read(path)
	if err != nil {
		b.log.Error("broker: failed to read binding info", err)
		return fmt.Errorf("failed to read binding info: %v", err)
	}
	if secret == nil {
		return nil
	}

	// Decode the binding info
	info := new(bindingInfo)
	if err := mapstructure.Decode(secret.Data, info); err != nil {
		b.log.Error("broker: failed to decode binding info", err)
		return fmt.Errorf("failed to decode binding info: %v", err)
	}

	// Determine when we should renew
	nextRenew := info.Renew.Add(time.Duration(info.LeaseDuration/2) * time.Second)
	now := time.Now().UTC()

	// Determine when we should first first
	var renewIn time.Duration
	if nextRenew.Before(now) {
		renewIn = 5 * time.Second // Schedule immediate renew
	} else {
		renewIn = nextRenew.Sub(now)
	}

	// Setup Renew timer
	info.timer = time.AfterFunc(renewIn, func() {
		b.handleRenew(info)
	})

	// Store the info
	b.bindLock.Lock()
	b.binds[bindingID] = info
	b.bindLock.Unlock()
	return nil
}

// Stop is used to shutdown the broker
func (b *Broker) Stop() error {
	b.runLock.Lock()
	defer b.runLock.Unlock()

	// Do nothing if shutdown
	if !b.running {
		return nil
	}

	// Stop all the renew timers
	b.bindLock.Lock()
	for _, info := range b.binds {
		info.timer.Stop()
	}
	b.bindLock.Unlock()

	b.running = false
	return nil
}

// handleRenew is used to handle renewing a token
func (b *Broker) handleRenew(info *bindingInfo) {
	// Attempt to renew the token
	auth := b.client.Auth().Token()
	secret, err := auth.Renew(info.ClientToken, 0)
	if err != nil {
		b.log.Error("broker: token renew failed", err)
	}

	// Setup Renew timer
	var renew time.Duration
	if secret != nil {
		renew = time.Duration(secret.Auth.LeaseDuration) / 2 * time.Second
	} else {
		renew = 30 * time.Second
	}
	info.timer = time.AfterFunc(renew, func() {
		b.handleRenew(info)
	})
}

func (b *Broker) Services(ctx context.Context) []brokerapi.Service {
	b.log.Debug("broker: providing services catalog")
	brokerID, err := uuid.GenerateUUID()
	if err != nil {
		b.log.Fatal("broker: failed to generate ID", err)
	}
	return []brokerapi.Service{
		brokerapi.Service{
			ID:            brokerID,
			Name:          VaultBrokerName,
			Description:   VaultBrokerDescription,
			Tags:          []string{},
			Bindable:      true,
			PlanUpdatable: false,
			Plans: []brokerapi.ServicePlan{
				brokerapi.ServicePlan{
					ID:          fmt.Sprintf("%s.%s", brokerID, VaultPlanName),
					Name:        VaultPlanName,
					Description: VaultPlanDescription,
					Free:        brokerapi.FreeValue(true),
				},
			},
		},
	}
}

// Provision is used to setup a new instance of Vault tenant. For each
// tenant we create a new Vault policy called "cf-instanceID". This is
// granted access to the service, space, and org contexts. We then create
// a token role called "cf-instanceID" which is periodic. Lastly, we mount
// the backends for the instance, and optionally for the space and org if
// they do not exist yet.
func (b *Broker) Provision(ctx context.Context, instanceID string, details brokerapi.ProvisionDetails, async bool) (brokerapi.ProvisionedServiceSpec, error) {
	b.log.Debug("provisioning new instance", lager.Data{
		"instance-id": instanceID,
		"org-id":      details.OrganizationGUID,
		"space-id":    details.SpaceGUID,
	})

	// Generate the new policy
	var buf bytes.Buffer
	inp := ServicePolicyTemplateInput{
		ServiceID:   instanceID,
		SpaceID:     details.SpaceGUID,
		SpacePolicy: "write",
		OrgID:       details.OrganizationGUID,
		OrgPolicy:   "read",
	}
	if err := GeneratePolicy(&buf, &inp); err != nil {
		b.log.Error("broker: failed to generate policy", err)
		return brokerapi.ProvisionedServiceSpec{}, fmt.Errorf("failed to generate policy: %v", err)
	}

	// Create the new policy
	policyName := "cf-" + instanceID
	sys := b.client.Sys()
	b.log.Info(fmt.Sprintf("broker: creating new policy: %s", policyName))
	if err := sys.PutPolicy(policyName, string(buf.Bytes())); err != nil {
		b.log.Error("broker: failed to create policy", err)
		return brokerapi.ProvisionedServiceSpec{}, fmt.Errorf("failed to create policy: %v", err)
	}

	// Create the new token role
	path := "/auth/token/roles/cf-" + instanceID
	data := map[string]interface{}{
		"allowed_policies": []string{policyName},
		"period":           VaultPeriodicTTL,
		"renewable":        true,
	}
	b.log.Info(fmt.Sprintf("broker: creating new token role: %s", path))
	if _, err := b.client.Logical().Write(path, data); err != nil {
		b.log.Error("broker: failed to create token role", err)
		return brokerapi.ProvisionedServiceSpec{}, fmt.Errorf("failed to create token role: %v", err)
	}

	// Determine the mounts we need
	mounts := map[string]string{
		"/cf/" + details.OrganizationGUID + "/secret": "generic",
		"/cf/" + details.SpaceGUID + "/secret":        "generic",
		"/cf/" + instanceID + "/secret":               "generic",
		"/cf/" + instanceID + "/transit":              "transit",
	}

	// Mount the backends
	b.log.Info(fmt.Sprintf("broker: setting up mounts: %#v", mounts))
	if err := b.IdempotentMount(mounts); err != nil {
		b.log.Error("broker: failed to setup mounts", err)
		return brokerapi.ProvisionedServiceSpec{}, fmt.Errorf("failed to setup mounts: %v", err)
	}

	// Done
	return brokerapi.ProvisionedServiceSpec{}, nil
}

// Deprovision is used to remove a tenant of Vault. We use this to
// remove all the backends of the tenant, delete the token role, and policy.
func (b *Broker) Deprovision(ctx context.Context, instanceID string, details brokerapi.DeprovisionDetails, async bool) (brokerapi.DeprovisionServiceSpec, error) {
	b.log.Debug("deprovisioning new instance", lager.Data{
		"instance-id": instanceID,
	})

	// Unmount the backends
	mounts := []string{
		"/cf/" + instanceID + "/secret",
		"/cf/" + instanceID + "/transit",
	}
	if err := b.IdempotentUnmount(mounts); err != nil {
		b.log.Error("broker: failed to remove mounts", err)
		return brokerapi.DeprovisionServiceSpec{}, fmt.Errorf("failed to remove mounts: %v", err)
	}

	// Delete the token role
	path := "/auth/token/roles/cf-" + instanceID
	b.log.Info(fmt.Sprintf("broker: deleting token role: %s", path))
	if _, err := b.client.Logical().Delete(path); err != nil {
		b.log.Error("broker: failed to delete token role", err)
		return brokerapi.DeprovisionServiceSpec{}, fmt.Errorf("failed to delete token role: %v", err)
	}

	// Delete the token policy
	policyName := "cf-" + instanceID
	b.log.Info(fmt.Sprintf("broker: deleting policy: %s", policyName))
	if err := b.client.Sys().DeletePolicy(policyName); err != nil {
		b.log.Error("broker: failed to delete policy", err)
		return brokerapi.DeprovisionServiceSpec{}, fmt.Errorf("failed to delete policy: %v", err)
	}

	// Done!
	return brokerapi.DeprovisionServiceSpec{}, nil
}

// Bind is used to attach a tenant of Vault to an application in CloudFoundry.
// This should create a credential that is used to authorize against Vault.
func (b *Broker) Bind(ctx context.Context, instanceID, bindingID string, details brokerapi.BindDetails) (brokerapi.Binding, error) {
	b.log.Debug("binding service", lager.Data{
		"binding-id":  bindingID,
		"instance-id": instanceID,
	})

	binding := brokerapi.Binding{}
	roleName := "cf-" + instanceID

	// Create the token
	renewable := true
	secret, err := b.client.Auth().Token().CreateWithRole(&api.TokenCreateRequest{
		Policies:    []string{roleName},
		Metadata:    map[string]string{"cf-instance-id": instanceID, "cf-binding-id": bindingID},
		DisplayName: "cf-bind-" + bindingID,
		Renewable:   &renewable,
	}, roleName)
	if err != nil {
		b.log.Error("broker: failed creating token", err)
		return binding, fmt.Errorf("failed creating token: %v", err)
	}
	if secret.Auth == nil {
		err = errors.New("secret as no auth")
		b.log.Error("failed creating secret", err)
		return binding, err
	}

	// Create a binding info object
	now := time.Now().UTC()
	expires := now.Add(time.Duration(secret.Auth.LeaseDuration) * time.Second)
	info := &bindingInfo{
		Binding:       bindingID,
		ClientToken:   secret.Auth.ClientToken,
		Accessor:      secret.Auth.Accessor,
		LeaseDuration: secret.Auth.LeaseDuration,
		Renew:         now,
		Expires:       expires,
	}

	// Store the token and metadata in the generic secret backend
	path := "cf/broker/" + instanceID + "/" + bindingID
	if _, err := b.client.Logical().Write(path, structs.Map(info)); err != nil {
		defer b.RevokeAccessor(secret.Auth.Accessor)
		b.log.Error("failed to commit to broker", err)
		return binding, err
	}

	// Setup Renew timer
	renew := time.Duration(secret.Auth.LeaseDuration) / 2 * time.Second
	info.timer = time.AfterFunc(renew, func() {
		b.handleRenew(info)
	})

	// Store the info
	b.bindLock.Lock()
	b.binds[bindingID] = info
	b.bindLock.Unlock()

	// Save the credentials
	binding.Credentials = map[string]string{
		"vault_token_accessor": secret.Auth.Accessor,
		"vault_token":          secret.Auth.ClientToken,
		"vault_path":           "cf/" + instanceID,
	}
	return binding, nil
}

// Unbind is used to detach an applicaiton from a tenant in Vault.
func (b *Broker) Unbind(ctx context.Context, instanceID, bindingID string, details brokerapi.UnbindDetails) error {
	b.log.Debug("unbinding service", lager.Data{
		"binding-id":  bindingID,
		"instance-id": instanceID,
	})

	// Read the binding info
	path := "cf/broker/" + instanceID + "/" + bindingID
	secret, err := b.client.Logical().Read(path)
	if err != nil {
		b.log.Error("broker: failed to read binding info", err)
		return fmt.Errorf("failed to read binding info: %v", err)
	}
	if secret == nil {
		b.log.Error("broker: missing binding info for unbind", err)
		return fmt.Errorf("missing binding info")
	}

	// Decode the binding info
	var info bindingInfo
	if err := mapstructure.Decode(secret.Data, &info); err != nil {
		b.log.Error("broker: failed to decode binding info", err)
		return fmt.Errorf("failed to decode binding info: %v", err)
	}

	// Revoke the token
	if err := b.RevokeAccessor(info.Accessor); err != nil {
		b.log.Error("broker: failed to revoke accessor", err)
		return fmt.Errorf("failed to revoke accessor: %v", err)
	}

	// Delete the binding info
	if _, err := b.client.Logical().Delete(path); err != nil {
		b.log.Error("broker: failed to delete binding info", err)
		return fmt.Errorf("failed to delete binding info: %v", err)
	}

	// Delete the bind if it exists, stop the renew timer
	b.bindLock.Lock()
	existing, ok := b.binds[bindingID]
	if ok {
		delete(b.binds, bindingID)
		existing.timer.Stop()
	}
	b.bindLock.Unlock()

	// Done
	return nil
}

// Not implemented, only used for multiple plans
func (b *Broker) Update(ctx context.Context, instanceID string, details brokerapi.UpdateDetails, async bool) (brokerapi.UpdateServiceSpec, error) {
	b.log.Debug("updating service", lager.Data{
		"instance-id": instanceID,
	})
	return brokerapi.UpdateServiceSpec{}, nil
}

// Not implemented, only used for async
func (b *Broker) LastOperation(ctx context.Context, instanceID, operationData string) (brokerapi.LastOperation, error) {
	b.log.Debug("returning last operation", lager.Data{
		"instance-id": instanceID,
	})
	return brokerapi.LastOperation{}, nil
}

// RevokeAccessor revokes the given token by accessor.
func (b *Broker) RevokeAccessor(a string) error {
	if err := b.client.Auth().Token().RevokeAccessor(a); err != nil {
		b.log.Error("failed revoking accessor", err)
		return err
	}
	return nil
}

// IdempotentMount takes a list of mounts and their desired paths and mounts the
// backend at that path. The key is the path and the value is the type of
// backend to mount.
func (b *Broker) IdempotentMount(m map[string]string) error {
	b.mountMutex.Lock()
	defer b.mountMutex.Unlock()
	result, err := b.client.Sys().ListMounts()
	if err != nil {
		return err
	}

	// Strip all leading and trailing things
	mounts := make(map[string]struct{})
	for k, _ := range result {
		k = strings.Trim(k, "/")
		mounts[k] = struct{}{}
	}

	for k, v := range m {
		k = strings.Trim(k, "/")
		if _, ok := mounts[k]; ok {
			continue
		}
		if err := b.client.Sys().Mount(k, &api.MountInput{
			Type: v,
		}); err != nil {
			return err
		}
	}
	return nil
}

// IdempotentUnmount takes a list of mount paths and removes them if and only
// if they currently exist.
func (b *Broker) IdempotentUnmount(l []string) error {
	b.mountMutex.Lock()
	defer b.mountMutex.Unlock()
	result, err := b.client.Sys().ListMounts()
	if err != nil {
		return err
	}

	// Strip all leading and trailing things
	mounts := make(map[string]struct{})
	for k, _ := range result {
		k = strings.Trim(k, "/")
		mounts[k] = struct{}{}
	}

	for _, k := range l {
		k = strings.Trim(k, "/")
		if _, ok := mounts[k]; !ok {
			continue
		}
		if err := b.client.Sys().Unmount(k); err != nil {
			return err
		}
	}
	return nil
}
