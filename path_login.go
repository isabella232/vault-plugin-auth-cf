package pcf

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/cloudfoundry-community/go-cfclient"
	"github.com/hashicorp/go-uuid"
	"github.com/hashicorp/vault-plugin-auth-pcf/models"
	"github.com/hashicorp/vault-plugin-auth-pcf/signatures"
	"github.com/hashicorp/vault-plugin-auth-pcf/util"
	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/cidrutil"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/pkg/errors"
)

func (b *backend) pathLogin() *framework.Path {
	return &framework.Path{
		Pattern: "login",
		Fields: map[string]*framework.FieldSchema{
			"role": {
				Required:     true,
				Type:         framework.TypeString,
				DisplayName:  "Role Name",
				DisplayValue: "internally-defined-role",
				Description:  "The name of the role to authenticate against.",
			},
			"certificate": {
				Required:    true,
				Type:        framework.TypeString,
				DisplayName: "Client Certificate",
				Description: "The full client certificate available at the CF_INSTANCE_CERT path on the PCF instance.",
			},
			"signing_time": {
				Required:     true,
				Type:         framework.TypeString,
				DisplayName:  "Signing Time",
				DisplayValue: "2006-01-02T15:04:05Z",
				Description:  "The date and time used to construct the signature.",
			},
			"signature": {
				Required:    true,
				Type:        framework.TypeString,
				DisplayName: "Signature",
				Description: "The signature generated by the client certificate's private key.",
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.operationLoginUpdate,
			},
		},
		HelpSynopsis:    pathLoginSyn,
		HelpDescription: pathLoginDesc,
	}
}

// operationLoginUpdate is called by those wanting to gain access to Vault.
// They present a client certificate that should have been issued by the pre-configured
// Certificate Authority, and a signature that should have been signed by the client cert's
// private key. If this holds true, there are additional checks verifying everything looks
// good before authentication is given.
func (b *backend) operationLoginUpdate(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	// Here, we intentionally swallow and log any detailed errors from failed authentication.
	// That's so attackers can't as easily progressively resolve issues.
	// If they're supposed to be using Vault, they can reach out to system administrators
	// for logs of the issue to debug it.
	resp, err := b.attemptLogin(ctx, req, data)
	if err != nil {
		// Provide a failure ID so it's easy to marry a particular API call with its server-side logs.
		u, _ := uuid.GenerateUUID()
		b.logger.Error(fmt.Sprintf("authentication failed, failure ID %s: %s", u, err))
		return logical.ErrorResponse(fmt.Sprintf("authentication failed, failure ID %s", u)), nil
	}
	return resp, nil
}

func (b *backend) attemptLogin(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	// Grab the time immediately for checking against the request's signingTime.
	timeReceived := time.Now().UTC()

	roleName := data.Get("role").(string)
	if roleName == "" {
		return nil, errors.New("'role-name' is required")
	}

	signature := data.Get("signature").(string)
	if signature == "" {
		return nil, errors.New("'signature' is required")
	}

	clientCertificate := data.Get("certificate").(string)
	if clientCertificate == "" {
		return nil, errors.New("'certificate' is required")
	}

	signingTimeRaw := data.Get("signing_time").(string)
	if signingTimeRaw == "" {
		return nil, errors.New("'signing_time' is required")
	}
	signingTime, err := parseTime(signingTimeRaw)
	if err != nil {
		return nil, err
	}

	// Ensure the signingTime it was signed is no more than 5 minutes in the past
	// or 30 seconds in the future. This is another guard against replay attacks
	// that takes over after 5 minutes.
	fiveMinutesAgo := timeReceived.Add(time.Minute * time.Duration(-5))
	thirtySecondsFromNow := timeReceived.Add(time.Second * time.Duration(30))
	if signingTime.Before(fiveMinutesAgo) {
		return nil, fmt.Errorf("request is too old; signed at %s but received request at %s; raw signing time is %s", signingTime, timeReceived, signingTimeRaw)
	}
	if signingTime.After(thirtySecondsFromNow) {
		return nil, fmt.Errorf("request is too far in the future; signed at %s but received request at %s; raw signing time is %s", signingTime, timeReceived, signingTimeRaw)
	}

	// Ensure the private key used to create the signature matches our client
	// certificate, and that it signed the same data as is presented in the body.
	// This offers some protection against MITM attacks.
	matchingCert, err := signatures.Verify(signature, &signatures.SignatureData{
		SigningTime: signingTime,
		Role:        roleName,
		Certificate: clientCertificate,
	})
	if err != nil {
		return nil, err
	}

	// Ensure the matching certificate was actually issued by the CA configured.
	// This protects against self-generated client certificates.
	config, err := config(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("no CA is configured for verifying client certificates")
	}
	verifyOpts, err := config.VerifyOpts()
	if err != nil {
		return nil, err
	}
	if _, err := matchingCert.Verify(verifyOpts); err != nil {
		return nil, err
	}

	// Read PCF's identity fields from the certificate.
	pcfCert, err := models.NewPCFCertificateFromx509(matchingCert)
	if err != nil {
		return nil, err
	}

	// Ensure the pcf certificate meets the role's constraints.
	role, err := getRole(ctx, req.Storage, roleName)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, errors.New("no matching role")
	}

	if err := b.validate(config, role, pcfCert, req.Connection.RemoteAddr); err != nil {
		return nil, err
	}

	// Everything checks out.
	return &logical.Response{
		Auth: &logical.Auth{
			Period:   role.Period,
			Policies: role.Policies,
			Metadata: map[string]string{
				"role":        roleName,
				"instance_id": pcfCert.InstanceID,
				"org_id":      pcfCert.OrgID,
				"app_id":      pcfCert.AppID,
				"space_id":    pcfCert.SpaceID,
				"ip_address":  pcfCert.IPAddress.String(),
			},
			DisplayName: pcfCert.InstanceID,
			LeaseOptions: logical.LeaseOptions{
				Renewable: true,
				TTL:       role.TTL,
				MaxTTL:    role.MaxTTL,
			},
			Alias: &logical.Alias{
				Name: pcfCert.AppID,
			},
			BoundCIDRs: role.BoundCIDRs,
		},
	}, nil
}

func (b *backend) pathLoginRenew(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	config, err := config(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("no configuration is available for reaching the PCF API")
	}

	roleName := req.Auth.Metadata["role"]
	if roleName == "" {
		return nil, errors.New("unable to retrieve role from metadata during renewal")
	}
	role, err := getRole(ctx, req.Storage, roleName)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, errors.New("no matching role")
	}

	// Reconstruct the certificate and ensure it still meets all constraints.
	pcfCert, err := models.NewPCFCertificate(
		req.Auth.Metadata["instance_id"],
		req.Auth.Metadata["org_id"],
		req.Auth.Metadata["space_id"],
		req.Auth.Metadata["app_id"],
		req.Auth.Metadata["ip_address"],
	)
	if err := b.validate(config, role, pcfCert, req.Connection.RemoteAddr); err != nil {
		return nil, err
	}

	resp := &logical.Response{Auth: req.Auth}
	resp.Auth.TTL = role.TTL
	resp.Auth.MaxTTL = role.MaxTTL
	resp.Auth.Period = role.Period
	return resp, nil
}

func (b *backend) validate(config *models.Configuration, role *models.RoleEntry, pcfCert *models.PCFCertificate, reqConnRemoteAddr string) error {
	if !role.DisableIPMatching {
		if !matchesIPAddress(reqConnRemoteAddr, pcfCert.IPAddress) {
			return errors.New("no matching IP address")
		}
	}
	if !meetsBoundConstraints(pcfCert.InstanceID, role.BoundInstanceIDs) {
		return fmt.Errorf("instance ID %s doesn't match role constraints of %s", pcfCert.InstanceID, role.BoundInstanceIDs)
	}
	if !meetsBoundConstraints(pcfCert.AppID, role.BoundAppIDs) {
		return fmt.Errorf("app ID %s doesn't match role constraints of %s", pcfCert.AppID, role.BoundAppIDs)
	}
	if !meetsBoundConstraints(pcfCert.OrgID, role.BoundOrgIDs) {
		return fmt.Errorf("org ID %s doesn't match role constraints of %s", pcfCert.OrgID, role.BoundOrgIDs)
	}
	if !meetsBoundConstraints(pcfCert.SpaceID, role.BoundSpaceIDs) {
		return fmt.Errorf("space ID %s doesn't match role constraints of %s", pcfCert.SpaceID, role.BoundSpaceIDs)
	}
	if !cidrutil.RemoteAddrIsOk(reqConnRemoteAddr, role.BoundCIDRs) {
		return fmt.Errorf("remote address %s doesn't match role constraints of %s", reqConnRemoteAddr, role.BoundCIDRs)
	}

	// Use the PCF API to ensure everything still exists and to verify whatever we can.
	client, err := cfclient.NewClient(&cfclient.Config{
		ApiAddress: config.PCFAPIAddr,
		Username:   config.PCFUsername,
		Password:   config.PCFPassword,
	})
	if err != nil {
		return err
	}

	// Check everything we can using the instance ID.
	serviceInstance, err := client.GetServiceInstanceByGuid(pcfCert.InstanceID)
	if err != nil {
		return err
	}
	if serviceInstance.Guid != pcfCert.InstanceID {
		return fmt.Errorf("cert instance ID %s doesn't match API's expected one of %s", pcfCert.InstanceID, serviceInstance.Guid)
	}
	if serviceInstance.SpaceGuid != pcfCert.SpaceID {
		return fmt.Errorf("cert space ID %s doesn't match API's expected one of %s", pcfCert.SpaceID, serviceInstance.SpaceGuid)
	}

	// Check everything we can using the app ID.
	app, err := client.AppByGuid(pcfCert.AppID)
	if err != nil {
		return err
	}
	if app.Guid != pcfCert.AppID {
		return fmt.Errorf("cert app ID %s doesn't match API's expected one of %s", pcfCert.AppID, app.Guid)
	}
	if app.SpaceGuid != pcfCert.SpaceID {
		return fmt.Errorf("cert space ID %s doesn't match API's expected one of %s", pcfCert.SpaceID, app.SpaceGuid)
	}
	if app.Instances <= 0 {
		return errors.New("app doesn't have any live instances")
	}

	// Check everything we can using the org ID.
	org, err := client.GetOrgByGuid(pcfCert.OrgID)
	if err != nil {
		return err
	}
	if org.Guid != pcfCert.OrgID {
		return fmt.Errorf("cert org ID %s doesn't match API's expected one of %s", pcfCert.OrgID, org.Guid)
	}

	// Check everything we can using the space ID.
	space, err := client.GetSpaceByGuid(pcfCert.SpaceID)
	if err != nil {
		return err
	}
	if space.Guid != pcfCert.SpaceID {
		return fmt.Errorf("cert space ID %s doesn't match API's expected one of %s", pcfCert.SpaceID, space.Guid)
	}
	if space.OrganizationGuid != pcfCert.OrgID {
		return fmt.Errorf("cert org ID %s doesn't match API's expected one of %s", pcfCert.OrgID, space.OrganizationGuid)
	}
	return nil
}

func meetsBoundConstraints(certValue string, constraints []string) bool {
	if len(constraints) == 0 {
		// There are no restrictions, so everything passes this check.
		return true
	}
	// Check whether we have a match.
	for _, p := range constraints {
		if p == certValue {
			return true
		}
	}
	return false
}

func matchesIPAddress(remoteAddr string, certIP net.IP) bool {
	// Some remote addresses may arrive like "10.255.181.105/32"
	// but the certificate will only have the IP address without
	// the subnet mask, so that's what we want to match against.
	// For those wanting to also match the subnet, use bound_cidrs.
	parts := strings.Split(remoteAddr, "/")
	reqIPAddr := net.ParseIP(parts[0])
	if certIP.Equal(reqIPAddr) {
		return true
	}
	return false
}

// Try parsing this as ISO 8601 AND the way that is default provided by Bash to make it easier to give via the CLI as well.
func parseTime(signingTime string) (time.Time, error) {
	if signingTime, err := time.Parse(signatures.TimeFormat, signingTime); err == nil {
		return signingTime, nil
	}
	if signingTime, err := time.Parse(util.BashTimeFormat, signingTime); err == nil {
		return signingTime, nil
	}
	return time.Time{}, fmt.Errorf("couldn't parse %s", signingTime)
}

const pathLoginSyn = `
Authenticates an entity with Vault.
`

const pathLoginDesc = `
Authenticate PCF entities using a client certificate issued by the 
configured Certificate Authority, and signed by a client key belonging
to the client certificate.
`
