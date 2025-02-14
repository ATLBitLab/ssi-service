package credential

import (
	"context"
	"fmt"
	"time"

	"github.com/TBD54566975/ssi-sdk/credential"
	schemalib "github.com/TBD54566975/ssi-sdk/credential/schema"
	statussdk "github.com/TBD54566975/ssi-sdk/credential/status"
	"github.com/TBD54566975/ssi-sdk/did"
	"github.com/TBD54566975/ssi-sdk/did/resolution"
	sdkutil "github.com/TBD54566975/ssi-sdk/util"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/tbd54566975/ssi-service/config"
	credint "github.com/tbd54566975/ssi-service/internal/credential"
	"github.com/tbd54566975/ssi-service/internal/keyaccess"
	"github.com/tbd54566975/ssi-service/internal/verification"
	"github.com/tbd54566975/ssi-service/pkg/server/pagination"
	"github.com/tbd54566975/ssi-service/pkg/service/framework"
	"github.com/tbd54566975/ssi-service/pkg/service/keystore"
	"github.com/tbd54566975/ssi-service/pkg/service/schema"
	"github.com/tbd54566975/ssi-service/pkg/storage"
	"go.einride.tech/aip/filtering"
)

type Service struct {
	storage  *Storage
	config   config.CredentialServiceConfig
	verifier *verification.Verifier

	// external dependencies
	keyStore *keystore.Service
	schema   *schema.Service
}

func (s Service) Type() framework.Type {
	return framework.Credential
}

func (s Service) Status() framework.Status {
	ae := sdkutil.NewAppendError()
	if s.storage == nil {
		ae.AppendString("no storage configured")
	}
	if s.verifier == nil {
		ae.AppendString("no credential verifier configured")
	}
	if s.keyStore == nil {
		ae.AppendString("no key store service configured")
	}
	if s.schema == nil {
		ae.AppendString("no schema service configured")
	}
	if !ae.IsEmpty() {
		return framework.Status{
			Status:  framework.StatusNotReady,
			Message: fmt.Sprintf("credential service is not ready: %s", ae.Error().Error()),
		}
	}
	return framework.Status{Status: framework.StatusReady}
}

func (s Service) Config() config.CredentialServiceConfig {
	return s.config
}

func NewCredentialService(config config.CredentialServiceConfig, s storage.ServiceStorage, keyStore *keystore.Service,
	didResolver resolution.Resolver, schema *schema.Service) (*Service, error) {
	credentialStorage, err := NewCredentialStorage(s)
	if err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "could not instantiate storage for the credential service")
	}
	verifier, err := verification.NewVerifiableDataVerifier(didResolver, schema)
	if err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "could not instantiate verifier for the credential service")
	}
	service := Service{
		storage:  credentialStorage,
		config:   config,
		verifier: verifier,
		keyStore: keyStore,
		schema:   schema,
	}
	if !service.Status().IsReady() {
		return nil, errors.New(service.Status().Message)
	}
	return &service, nil
}

func (s Service) CreateCredential(ctx context.Context, request CreateCredentialRequest) (*CreateCredentialResponse, error) {
	if err := request.IsValid(); err != nil {
		return nil, errors.Wrap(err, "validating request")
	}

	watchKeys := make([]storage.WatchKey, 0)

	var statusMetadata StatusListCredentialMetadata
	if request.hasStatus() && request.isStatusValid() {
		statusPurpose := statussdk.StatusRevocation

		if request.Suspendable {
			statusPurpose = statussdk.StatusSuspension
		}

		statusListCredentialWatchKey := s.storage.GetStatusListCredentialWatchKey(request.Issuer, request.SchemaID, string(statusPurpose))
		statusListCredentialIndexPoolWatchKey := s.storage.GetStatusListIndexPoolWatchKey(request.Issuer, request.SchemaID, string(statusPurpose))
		statusListCredentialCurrentIndexWatchKey := s.storage.GetStatusListCurrentIndexWatchKey(request.Issuer, request.SchemaID, string(statusPurpose))

		watchKeys = append(watchKeys, statusListCredentialWatchKey)
		watchKeys = append(watchKeys, statusListCredentialIndexPoolWatchKey)
		watchKeys = append(watchKeys, statusListCredentialCurrentIndexWatchKey)

		statusMetadata = StatusListCredentialMetadata{statusListCredentialWatchKey: statusListCredentialWatchKey, statusListIndexPoolWatchKey: statusListCredentialIndexPoolWatchKey, statusListCurrentIndexWatchKey: statusListCredentialCurrentIndexWatchKey}
	}

	returnFunc := s.createCredentialFunc(request, statusMetadata)
	returnValue, err := s.storage.db.Execute(ctx, returnFunc, watchKeys)
	if err != nil {
		return nil, errors.Wrap(err, "execute")
	}

	credResponse, ok := returnValue.(*CreateCredentialResponse)
	if !ok {
		return nil, errors.New("problem casting to CreateCredentialResponse")
	}

	return credResponse, nil
}

func (s Service) createCredentialFunc(request CreateCredentialRequest, slcMetadata StatusListCredentialMetadata) storage.BusinessLogicFunc {
	return func(ctx context.Context, tx storage.Tx) (any, error) {
		return s.createCredential(ctx, request, tx, slcMetadata)
	}
}

func (s Service) createCredential(ctx context.Context, request CreateCredentialRequest, tx storage.Tx, statusMetadata StatusListCredentialMetadata) (*CreateCredentialResponse, error) {
	logrus.Debugf("creating credential: %+v", request)

	if !request.isStatusValid() {
		return nil, sdkutil.LoggingNewError("credential may have at most one status")
	}

	builder := credential.NewVerifiableCredentialBuilder()
	credentialID := uuid.NewString()
	credentialURI := config.GetServicePath(framework.Credential) + "/" + credentialID
	if err := builder.SetID(credentialURI); err != nil {
		return nil, sdkutil.LoggingErrorMsgf(err, "could not build credential when setting id: %s", credentialURI)
	}

	if err := builder.SetIssuer(request.Issuer); err != nil {
		return nil, sdkutil.LoggingErrorMsgf(err, "could not build credential when setting issuer: %s", request.Issuer)
	}

	// check if there's a conflict with subject ID
	if id, ok := request.Data[credential.VerifiableCredentialIDProperty]; ok && id != request.Subject {
		return nil, sdkutil.LoggingNewErrorf("cannot set subject<%s>, data already contains a different ID value: %s", request.Subject, id)
	}

	// set subject value
	subject := credential.CredentialSubject(request.Data)
	subject[credential.VerifiableCredentialIDProperty] = request.Subject
	if err := builder.SetCredentialSubject(subject); err != nil {
		return nil, sdkutil.LoggingErrorMsgf(err, "could not set subject: %+v", subject)
	}

	// if a context value exists, set it
	if request.Context != "" {
		if err := builder.AddContext(request.Context); err != nil {
			return nil, sdkutil.LoggingErrorMsgf(err, "could not add context to credential: %s", request.Context)
		}
	}

	// if a schema value exists, verify we can access it, validate the data against it, then set it
	var knownSchema *schemalib.JSONSchema
	if request.SchemaID != "" {
		// resolve schema and save it for validation later
		gotSchema, schemaType, err := s.schema.Resolve(ctx, request.SchemaID)
		if err != nil {
			return nil, sdkutil.LoggingErrorMsgf(err, "failed to create credential; could not get schema: %s", request.SchemaID)
		}
		knownSchema = gotSchema
		credSchema := credential.CredentialSchema{
			ID:   request.SchemaID,
			Type: schemaType.String(),
		}
		if err = builder.SetCredentialSchema(credSchema); err != nil {
			return nil, sdkutil.LoggingErrorMsgf(err, "could not set JSON Schema for credential: %s", request.SchemaID)
		}
	}

	// if an expiry value exists, set it
	if request.Expiry != "" {
		if err := builder.SetExpirationDate(request.Expiry); err != nil {
			return nil, sdkutil.LoggingErrorMsgf(err, "could not set expiry for credential: %s", request.Expiry)
		}
	}

	if err := builder.SetIssuanceDate(time.Now().Format(time.RFC3339)); err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "could not set credential issuance date")
	}

	if request.hasStatus() {
		statusEntry, err := s.createStatusListEntryForCredential(ctx, builder.ID, request, tx, statusMetadata)
		if err != nil {
			return nil, sdkutil.LoggingErrorMsg(err, "could not create status list entry for credential")
		}
		if err = builder.SetCredentialStatus(statusEntry); err != nil {
			return nil, sdkutil.LoggingErrorMsg(err, "could not set credential status")
		}
	}

	if request.hasEvidence() {
		if err := request.validateEvidence(); err != nil {
			return nil, sdkutil.LoggingErrorMsg(err, "validating evidence")
		}

		if err := builder.SetEvidence(request.Evidence); err != nil {
			return nil, sdkutil.LoggingErrorMsg(err, "could not set evidence")
		}
	}

	cred, err := builder.Build()
	if err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "could not build credential")
	}

	// verify the built schema complies with the schema we've set
	if knownSchema != nil {
		if err = schemalib.IsCredentialValidForJSONSchema(*cred, *knownSchema); err != nil {
			return nil, sdkutil.LoggingErrorMsgf(err, "credential data does not comply with the provided schema: %s", request.SchemaID)
		}
	}

	// TODO(gabe) support Data Integrity creds too https://github.com/TBD54566975/ssi-service/issues/105
	credCopy, err := credint.CopyCredential(*cred)
	if err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "could not copy credential")
	}
	credJWT, err := s.signCredentialJWT(ctx, request.FullyQualifiedVerificationMethodID, *credCopy)
	if err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "signing credential")
	}

	container := credint.Container{
		ID:                                 credentialID,
		FullyQualifiedVerificationMethodID: request.FullyQualifiedVerificationMethodID,
		Credential:                         cred,
		CredentialJWT:                      credJWT,
		Revoked:                            false,
		Suspended:                          false,
	}

	credentialStorageRequest := StoreCredentialRequest{
		Container: container,
	}
	if err = s.storage.StoreCredentialTx(ctx, tx, credentialStorageRequest); err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "saving credential")
	}

	return &CreateCredentialResponse{Container: container}, nil
}

// signCredentialJWT signs a credential and returns it as a vc-jwt
func (s Service) signCredentialJWT(ctx context.Context, verificationMethodID string, cred credential.VerifiableCredential) (*keyaccess.JWT, error) {
	keyStoreID := did.FullyQualifiedVerificationMethodID(cred.IssuerID(), verificationMethodID)
	gotKey, err := s.keyStore.GetKey(ctx, keystore.GetKeyRequest{ID: keyStoreID})
	if err != nil {
		return nil, sdkutil.LoggingErrorMsgf(err, "getting key for signing credential<%s>", verificationMethodID)
	}
	if gotKey.Controller != cred.Issuer.(string) {
		return nil, sdkutil.LoggingNewErrorf("key controller<%s> does not match credential issuer<%s> for key<%s>", gotKey.Controller, cred.Issuer, verificationMethodID)
	}
	if gotKey.Revoked {
		return nil, sdkutil.LoggingNewErrorf("cannot use revoked key<%s>", gotKey.ID)
	}
	keyAccess, err := keyaccess.NewJWKKeyAccess(verificationMethodID, gotKey.ID, gotKey.Key)
	if err != nil {
		return nil, errors.Wrapf(err, "creating key access for signing credential with key<%s>", gotKey.ID)
	}
	credToken, err := keyAccess.SignVerifiableCredential(cred)
	if err != nil {
		return nil, errors.Wrapf(err, "could not sign credential with key<%s>", gotKey.ID)
	}
	return credToken, nil
}

type VerifyCredentialRequest struct {
	DataIntegrityCredential *credential.VerifiableCredential `json:"credential,omitempty"`
	CredentialJWT           *keyaccess.JWT                   `json:"credentialJwt,omitempty"`
}

// IsValid checks if the request is valid, meaning there is at least one data integrity (with proof)
// OR jwt credential, but not both
func (vcr VerifyCredentialRequest) IsValid() error {
	if vcr.DataIntegrityCredential == nil && vcr.CredentialJWT == nil {
		return errors.New("either a credential or a credential JWT must be provided")
	}
	if (vcr.DataIntegrityCredential != nil && vcr.DataIntegrityCredential.Proof != nil) && vcr.CredentialJWT != nil {
		return errors.New("only one of credential or credential JWT can be provided")
	}
	return nil
}

type VerifyCredentialResponse struct {
	Verified bool   `json:"verified"`
	Reason   string `json:"reason,omitempty"`
}

// VerifyCredential does three levels of verification on a credential:
// 1. Makes sure the credential has a valid signature
// 2. Makes sure the credential has is not expired
// 3. Makes sure the credential complies with the VC Data Model
// 4. If the credential has a schema, makes sure its data complies with the schema
// LATER: Makes sure the credential has not been revoked, other checks.
func (s Service) VerifyCredential(ctx context.Context, request VerifyCredentialRequest) (*VerifyCredentialResponse, error) {
	logrus.Debugf("verifying credential: %+v", request)

	if err := request.IsValid(); err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "invalid verify credential request")
	}

	if request.CredentialJWT != nil {
		err := s.verifier.VerifyJWTCredential(ctx, *request.CredentialJWT)
		if err != nil {
			return &VerifyCredentialResponse{Verified: false, Reason: err.Error()}, nil
		}
	} else {
		if err := s.verifier.VerifyDataIntegrityCredential(ctx, *request.DataIntegrityCredential); err != nil {
			return &VerifyCredentialResponse{Verified: false, Reason: err.Error()}, nil
		}
	}

	return &VerifyCredentialResponse{Verified: true}, nil
}

func (s Service) GetCredential(ctx context.Context, request GetCredentialRequest) (*GetCredentialResponse, error) {
	logrus.Debugf("getting credential: %s", request.ID)

	gotCred, err := s.storage.GetCredential(ctx, request.ID)
	if err != nil {
		return nil, sdkutil.LoggingErrorMsgf(err, "could not get credential: %s", request.ID)
	}
	if !gotCred.IsValid() {
		return nil, sdkutil.LoggingNewErrorf("credential returned is not valid: %s", request.ID)
	}
	response := GetCredentialResponse{
		credint.Container{
			ID:            gotCred.LocalCredentialID,
			Credential:    gotCred.Credential,
			CredentialJWT: gotCred.CredentialJWT,
			Revoked:       gotCred.Revoked,
			Suspended:     gotCred.Suspended,
		},
	}
	return &response, nil
}

func (s Service) ListCredentials(ctx context.Context, filter filtering.Filter, request pagination.PageRequest) (*ListCredentialsResponse, error) {
	logrus.Debugf("listing credential(s) ")

	gotCreds, err := s.storage.ListCredentials(ctx, filter, request.ToServicePage())
	if err != nil {
		return nil, sdkutil.LoggingErrorMsgf(err, "could not list credential(s)")
	}

	creds := make([]credint.Container, 0, len(gotCreds.StoredCredentials))
	for _, cred := range gotCreds.StoredCredentials {
		container := credint.Container{
			ID:            cred.LocalCredentialID,
			Credential:    cred.Credential,
			CredentialJWT: cred.CredentialJWT,
			Revoked:       cred.Revoked,
			Suspended:     cred.Suspended,
		}
		creds = append(creds, container)
	}

	response := ListCredentialsResponse{
		Credentials:   creds,
		NextPageToken: gotCreds.NextPageToken,
	}
	return &response, nil
}

func (s Service) GetCredentialStatus(ctx context.Context, request GetCredentialStatusRequest) (*GetCredentialStatusResponse, error) {
	logrus.Debugf("getting credential status: %s", request.ID)

	gotCred, err := s.storage.GetCredential(ctx, request.ID)
	if err != nil {
		return nil, sdkutil.LoggingErrorMsgf(err, "could not get credential: %s", request.ID)
	}
	if !gotCred.IsValid() {
		return nil, sdkutil.LoggingNewErrorf("credential returned is not valid: %s", request.ID)
	}
	response := GetCredentialStatusResponse{
		Revoked:   gotCred.Revoked,
		Suspended: gotCred.Suspended,
	}
	return &response, nil
}

func (s Service) GetCredentialStatusList(ctx context.Context, request GetCredentialStatusListRequest) (*GetCredentialStatusListResponse, error) {
	logrus.Debugf("getting credential status list: %s", request.ID)

	gotCred, err := s.storage.GetStatusListCredential(ctx, request.ID)
	if err != nil {
		return nil, sdkutil.LoggingErrorMsgf(err, "could not get credential: %s", request.ID)
	}
	if !gotCred.IsValid() {
		return nil, sdkutil.LoggingNewErrorf("credential returned is not valid: %s", request.ID)
	}
	response := GetCredentialStatusListResponse{
		credint.Container{
			ID:            gotCred.LocalCredentialID,
			Credential:    gotCred.Credential,
			CredentialJWT: gotCred.CredentialJWT,
			Revoked:       false, // Credential Status List cannot be revoked
			Suspended:     false, // Credential Status List cannot be suspended
		},
	}
	return &response, nil
}

func (s Service) UpdateCredentialStatus(ctx context.Context, request UpdateCredentialStatusRequest) (*UpdateCredentialStatusResponse, error) {

	statusListCredentialWatchKey, err := s.statusListCredentialWatchKey(ctx, request.ID)
	if err != nil {
		return nil, err
	}

	slcMetadata := StatusListCredentialMetadata{statusListCredentialWatchKey: *statusListCredentialWatchKey}
	watchKeys := []storage.WatchKey{*statusListCredentialWatchKey}
	returnFunc := s.updateCredentialStatusFunc(request, slcMetadata)

	returnValue, err := s.storage.db.Execute(ctx, returnFunc, watchKeys)
	if err != nil {
		return nil, errors.Wrap(err, "execute")
	}

	credResponse, ok := returnValue.(*UpdateCredentialStatusResponse)
	if !ok {
		return nil, errors.New("casting to UpdateCredentialStatusResponse")
	}

	return credResponse, nil
}

func (s Service) updateCredentialStatusFunc(request UpdateCredentialStatusRequest, slcMetadata StatusListCredentialMetadata) storage.BusinessLogicFunc {
	return func(ctx context.Context, tx storage.Tx) (any, error) {
		return s.updateCredentialStatusBusinessLogic(ctx, tx, request, slcMetadata)
	}
}

func (s Service) updateCredentialStatusBusinessLogic(ctx context.Context, tx storage.Tx, request UpdateCredentialStatusRequest, slcMetadata StatusListCredentialMetadata) (*UpdateCredentialStatusResponse, error) {
	logrus.Debugf("updating credential status: %s to Revoked: %v, Suspended: %v", request.ID, request.Revoked, request.Suspended)

	if request.Suspended && request.Revoked {
		return nil, sdkutil.LoggingNewErrorf("cannot update both suspended and revoked status")
	}

	gotCred, err := s.storage.GetCredential(ctx, request.ID)
	if err != nil {
		return nil, sdkutil.LoggingErrorMsgf(err, "could not get credential: %s", request.ID)
	}
	if !gotCred.IsValid() {
		return nil, sdkutil.LoggingNewErrorf("credential returned is not valid: %s", request.ID)
	}

	// if the request is the same as what the current credential is there is no action
	if gotCred.Revoked == request.Revoked && gotCred.Suspended == request.Suspended {
		logrus.Warn("request and credential have same status, no action is needed")
		response := UpdateCredentialStatusResponse{Status{
			Revoked:   gotCred.Revoked,
			Suspended: gotCred.Suspended,
		}}
		return &response, nil
	}

	container, err := updateCredentialStatus(ctx, tx, s, gotCred, request, slcMetadata)
	if err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "updating credential")
	}

	response := UpdateCredentialStatusResponse{Status{Revoked: container.Revoked, Suspended: container.Suspended}}
	return &response, nil
}

func updateCredentialStatus(ctx context.Context, tx storage.Tx, s Service, gotCred *StoredCredential, request UpdateCredentialStatusRequest, slcMetadata StatusListCredentialMetadata) (*credint.Container, error) {
	// store the credential with updated status
	container := credint.Container{
		ID:                                 gotCred.LocalCredentialID,
		FullyQualifiedVerificationMethodID: gotCred.FullyQualifiedVerificationMethodID,
		Credential:                         gotCred.Credential,
		CredentialJWT:                      gotCred.CredentialJWT,
		Revoked:                            request.Revoked,
		Suspended:                          request.Suspended,
	}

	storageRequest := StoreCredentialRequest{
		Container: container,
	}

	if err := s.storage.StoreCredentialTx(ctx, tx, storageRequest); err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "could not store credential")
	}

	statusListCredentialURI := gotCred.Credential.CredentialStatus.(map[string]any)["statusListCredential"].(string)

	if len(statusListCredentialURI) == 0 {
		return nil, sdkutil.LoggingNewErrorf("problem with getting status list credential id")
	}

	statusListCredentialID, err := parseIDFromURI(statusListCredentialURI)
	if err != nil {
		return nil, err
	}

	creds, err := s.storage.GetCredentialsByIssuerAndSchema(ctx, gotCred.Issuer, gotCred.Schema)
	if err != nil {
		return nil, sdkutil.LoggingNewErrorf("problem with getting status list credential for issuer: %s schema: %s", gotCred.Issuer, gotCred.Schema)
	}

	var revokedOrSuspendedStatusCreds []credential.VerifiableCredential
	for _, cred := range creds {
		// we add the current cred to the creds list based on request, not on what could be in stale database that the tx has not updated yet
		if cred.Credential.ID == gotCred.Credential.ID {
			continue
		}

		if request.Revoked && cred.Credential.CredentialStatus != nil && cred.Revoked {
			revokedOrSuspendedStatusCreds = append(revokedOrSuspendedStatusCreds, *cred.Credential)
		} else if request.Suspended && cred.Credential.CredentialStatus != nil && cred.Suspended {
			revokedOrSuspendedStatusCreds = append(revokedOrSuspendedStatusCreds, *cred.Credential)
		}
	}

	// add current one since it has not been saved yet and won't be available in the creds array
	if request.Revoked == true || request.Suspended == true {
		revokedOrSuspendedStatusCreds = append(revokedOrSuspendedStatusCreds, *gotCred.Credential)
	}

	statusPurpose := statussdk.StatusRevocation

	if request.Suspended {
		statusPurpose = statussdk.StatusSuspension
	}

	generatedStatusListCredential, err := statussdk.GenerateStatusList2021Credential(statusListCredentialURI, gotCred.Issuer, statusPurpose, revokedOrSuspendedStatusCreds)
	if err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "could not generate status list")
	}

	generatedStatusListCredential.CredentialSchema = gotCred.Credential.CredentialSchema

	statusListCredJWT, err := s.signCredentialJWT(ctx, gotCred.FullyQualifiedVerificationMethodID, *generatedStatusListCredential)
	if err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "could not sign status list credential")
	}

	// store the status list credential
	statusListContainer := credint.Container{
		ID:                                 statusListCredentialID,
		FullyQualifiedVerificationMethodID: gotCred.FullyQualifiedVerificationMethodID,
		Credential:                         generatedStatusListCredential,
		CredentialJWT:                      statusListCredJWT,
	}

	storageRequest = StoreCredentialRequest{
		Container: statusListContainer,
	}

	if err = s.storage.StoreStatusListCredentialTx(ctx, tx, storageRequest, slcMetadata); err != nil {
		return nil, sdkutil.LoggingErrorMsg(err, "could not store credential status list")
	}

	return &container, nil
}

func parseIDFromURI(uri string) (string, error) {
	const uuidStandardFormLen = 36
	if len(uri) < uuidStandardFormLen {
		return "", sdkutil.LoggingNewErrorf("cannot infer status list credential id from %q", uri)
	}
	return uri[len(uri)-uuidStandardFormLen:], nil
}

func (s Service) DeleteCredential(ctx context.Context, request DeleteCredentialRequest) error {

	logrus.Debugf("deleting credential: %s", request.ID)

	if err := s.storage.DeleteCredential(ctx, request.ID); err != nil {
		return sdkutil.LoggingErrorMsgf(err, "deleting credential with id: %s", request.ID)
	}

	return nil
}

func (s Service) BatchCreateCredentials(ctx context.Context, batchRequest BatchCreateCredentialsRequest) (*BatchCreateCredentialsResponse, error) {
	watchKeys := make([]storage.WatchKey, 0, len(batchRequest.Requests)*3)

	funcs := make([]storage.BusinessLogicFunc, 0, len(batchRequest.Requests))
	for _, request := range batchRequest.Requests {
		var statusMetadata StatusListCredentialMetadata
		if request.hasStatus() && request.isStatusValid() {
			statusPurpose := statussdk.StatusRevocation

			if request.Suspendable {
				statusPurpose = statussdk.StatusSuspension
			}

			statusListCredentialWatchKey := s.storage.GetStatusListCredentialWatchKey(request.Issuer, request.SchemaID, string(statusPurpose))
			statusListCredentialIndexPoolWatchKey := s.storage.GetStatusListIndexPoolWatchKey(request.Issuer, request.SchemaID, string(statusPurpose))
			statusListCredentialCurrentIndexWatchKey := s.storage.GetStatusListCurrentIndexWatchKey(request.Issuer, request.SchemaID, string(statusPurpose))

			watchKeys = append(watchKeys, statusListCredentialWatchKey)
			watchKeys = append(watchKeys, statusListCredentialIndexPoolWatchKey)
			watchKeys = append(watchKeys, statusListCredentialCurrentIndexWatchKey)

			statusMetadata = StatusListCredentialMetadata{
				statusListCredentialWatchKey:   statusListCredentialWatchKey,
				statusListIndexPoolWatchKey:    statusListCredentialIndexPoolWatchKey,
				statusListCurrentIndexWatchKey: statusListCredentialCurrentIndexWatchKey,
			}
		}

		funcs = append(funcs, s.createCredentialFunc(request, statusMetadata))
	}

	returnFunc := storage.BusinessLogicFunc(func(ctx context.Context, tx storage.Tx) (any, error) {
		resp := new(BatchCreateCredentialsResponse)
		resp.Credentials = make([]credint.Container, len(batchRequest.Requests))
		for i, f := range funcs {
			credRespAny, err := f(ctx, tx)
			if err != nil {
				return nil, err
			}
			credResp, ok := credRespAny.(*CreateCredentialResponse)
			if !ok {
				return nil, errors.New("problem casting to CreateCredentialResponse")
			}
			resp.Credentials[i] = credResp.Container
		}
		return resp, nil
	})

	returnValue, err := s.storage.db.Execute(ctx, returnFunc, watchKeys)
	if err != nil {
		return nil, errors.Wrap(err, "execute")
	}

	credResponse, ok := returnValue.(*BatchCreateCredentialsResponse)
	if !ok {
		return nil, errors.New("problem casting to BatchCreateCredentialsResponse")
	}

	return credResponse, nil
}

func (s Service) BatchUpdateCredentialStatus(ctx context.Context, batchRequest BatchUpdateCredentialStatusRequest) (*BatchUpdateCredentialStatusResponse, error) {
	watchKeys := make([]storage.WatchKey, 0, len(batchRequest.Requests))
	updateFuncs := make([]storage.BusinessLogicFunc, 0, len(batchRequest.Requests))
	for _, request := range batchRequest.Requests {
		statusListCredentialWatchKey, err := s.statusListCredentialWatchKey(ctx, request.ID)
		if err != nil {
			return nil, err
		}
		watchKeys = append(watchKeys, *statusListCredentialWatchKey)

		slcMetadata := StatusListCredentialMetadata{statusListCredentialWatchKey: *statusListCredentialWatchKey}
		returnFunc := s.updateCredentialStatusFunc(request, slcMetadata)
		updateFuncs = append(updateFuncs, returnFunc)
	}
	returnValue, err := s.storage.db.Execute(ctx, func(ctx context.Context, tx storage.Tx) (any, error) {
		batchResponse := BatchUpdateCredentialStatusResponse{
			CredentialStatuses: make([]Status, 0, len(batchRequest.Requests)),
		}
		for i, updateFunc := range updateFuncs {
			updateResp, err := updateFunc(ctx, tx)
			if err != nil {
				return nil, err
			}
			batchResponse.CredentialStatuses = append(batchResponse.CredentialStatuses, updateResp.(*UpdateCredentialStatusResponse).Status)
			batchResponse.CredentialStatuses[i].ID = batchRequest.Requests[i].ID
		}
		return &batchResponse, nil
	}, watchKeys)
	if err != nil {
		return nil, errors.Wrap(err, "execute")
	}

	batchResponse, ok := returnValue.(*BatchUpdateCredentialStatusResponse)
	if !ok {
		return nil, errors.New("casting to BatchUpdateCredentialStatusResponse")
	}

	return batchResponse, nil
}

func (s Service) statusListCredentialWatchKey(ctx context.Context, id string) (*storage.WatchKey, error) {
	gotCred, err := s.storage.GetCredential(ctx, id)
	if err != nil {
		return nil, errors.Wrap(err, "reading credential")
	}

	if !gotCred.HasCredentialStatus() {
		return nil, sdkutil.LoggingNewErrorf("credential %q has no credentialStatus field", gotCred.LocalCredentialID)
	}

	statusPurpose := gotCred.GetStatusPurpose()
	if len(statusPurpose) == 0 {
		return nil, sdkutil.LoggingNewErrorf("status purpose could not be derived from credential status")
	}

	statusListCredential, err := s.storage.GetStatusListCredentialKeyData(ctx, gotCred.Issuer, gotCred.Schema, statussdk.StatusPurpose(statusPurpose))
	if err != nil {
		return nil, errors.Wrap(err, "getting status list watch key uuid data")
	}

	if statusListCredential == nil {
		return nil, errors.Wrap(err, "status list credential should exist in order to update")
	}

	statusListCredentialWatchKey := s.storage.GetStatusListCredentialWatchKey(gotCred.Issuer, gotCred.Schema, statusPurpose)
	return &statusListCredentialWatchKey, nil
}
