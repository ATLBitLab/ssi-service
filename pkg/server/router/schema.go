package router

import (
	"fmt"
	"net/http"

	schemalib "github.com/TBD54566975/ssi-sdk/credential/schema"
	"github.com/TBD54566975/ssi-sdk/did"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"

	"github.com/tbd54566975/ssi-service/internal/keyaccess"
	"github.com/tbd54566975/ssi-service/pkg/server/framework"
	"github.com/tbd54566975/ssi-service/pkg/server/pagination"
	svcframework "github.com/tbd54566975/ssi-service/pkg/service/framework"
	"github.com/tbd54566975/ssi-service/pkg/service/schema"
)

type SchemaRouter struct {
	service *schema.Service
}

func NewSchemaRouter(s svcframework.Service) (*SchemaRouter, error) {
	if s == nil {
		return nil, errors.New("service cannot be nil")
	}
	schemaService, ok := s.(*schema.Service)
	if !ok {
		return nil, fmt.Errorf("could not create schema router with service type: %s", s.Type())
	}
	return &SchemaRouter{service: schemaService}, nil
}

type CreateSchemaRequest struct {
	// Name is a human-readable name for a schema
	Name string `json:"name" validate:"required"`
	// Description is an optional human-readable description for a schema
	Description string `json:"description,omitempty"`
	// Schema represents the JSON schema for the credential schema
	// If the schema has an $id field, it will be overwritten with an ID the service generates.
	// The schema must be against draft 2020-12, 2019-09, or 7.
	// Must include a string field `$schema` that must be one of `https://json-schema.org/draft/2020-12/schema`,
	// `https://json-schema.org/draft/2019-09/schema`, or `https://json-schema.org/draft-07/schema`.
	Schema schemalib.JSONSchema `json:"schema" validate:"required"`

	// CredentialSchemaRequest request is an optional additional request to create a credentialized version of a schema.
	*CredentialSchemaRequest
}

// CredentialSchemaRequest request is an optional additional request to create a credentialized version of a schema.
type CredentialSchemaRequest struct {
	// Issuer represents the DID of the issuer for the schema if it's signed. Required if intending to sign the
	// schema as a credential using JsonSchemaCredential.
	Issuer string `json:"issuer,omitempty" validate:"required"`

	// The id of the verificationMethod (see https://www.w3.org/TR/did-core/#verification-methods) who's privateKey is
	// stored in ssi-service. The verificationMethod must be part of the did document associated with `issuer`.
	// The private key associated with the verificationMethod's publicKey will be used to sign the schema as a
	// credential using JsonSchemaCredential.
	// Required if intending to sign the schema as a credential using JsonSchemaCredential.
	VerificationMethodID string `json:"verificationMethodId" validate:"required" example:"did:key:z6MkkZDjunoN4gyPMx5TSy7Mfzw22D2RZQZUcx46bii53Ex3#z6MkkZDjunoN4gyPMx5TSy7Mfzw22D2RZQZUcx46bii53Ex3"`
}

func (csr *CredentialSchemaRequest) IsValid() bool {
	if csr == nil {
		return false
	}
	return csr.Issuer != "" && csr.VerificationMethodID != ""
}

type CreateSchemaResponse struct {
	*SchemaResponse
}

type SchemaResponse struct {
	// ID is the URL of for resolution of the schema
	ID string `json:"id"`
	// Type is the type of schema such as `JsonSchema` or `JsonSchemaCredential`
	Type schemalib.VCJSONSchemaType `json:"type" validate:"required"`

	// Schema is the JSON schema for the credential, returned when the type is JsonSchema
	Schema *schemalib.JSONSchema `json:"schema,omitempty"`

	// CredentialSchema is the JWT schema for the credential, returned when the type is CredentialSchema
	CredentialSchema *keyaccess.JWT `json:"credentialSchema,omitempty"`
}

// CreateSchema godoc
//
//	@Summary		Create a Credential Schema
//	@Description	Create a schema for use with a Verifiable Credential
//	@Tags			Schemas
//	@Accept			json
//	@Produce		json
//	@Param			request	body		CreateSchemaRequest	true	"request body"
//	@Success		201		{object}	CreateSchemaResponse
//	@Failure		400		{string}	string	"Bad request"
//	@Failure		500		{string}	string	"Internal server error"
//	@Router			/v1/schemas [put]
func (sr SchemaRouter) CreateSchema(c *gin.Context) {
	var request CreateSchemaRequest
	invalidCreateSchemaRequest := "invalid create schema request"
	if err := framework.Decode(c.Request, &request); err != nil {
		framework.LoggingRespondErrWithMsg(c, err, invalidCreateSchemaRequest, http.StatusBadRequest)
		return
	}

	if err := framework.ValidateRequest(request); err != nil {
		framework.LoggingRespondErrWithMsg(c, err, invalidCreateSchemaRequest, http.StatusBadRequest)
		return
	}

	req := schema.CreateSchemaRequest{
		Name:        request.Name,
		Description: request.Description,
		Schema:      request.Schema,
	}

	if request.CredentialSchemaRequest != nil {
		if !request.CredentialSchemaRequest.IsValid() {
			errMsg := "cannot sign schema without an issuer DID and KID"
			framework.LoggingRespondErrMsg(c, errMsg, http.StatusBadRequest)
			return
		}
		// if we have a valid credential schema request, set the issuer and kid properties
		req.Issuer = request.Issuer
		req.FullyQualifiedVerificationMethodID = did.FullyQualifiedVerificationMethodID(request.Issuer, request.VerificationMethodID)
	}

	createSchemaResponse, err := sr.service.CreateSchema(c, req)
	if err != nil {
		framework.LoggingRespondErrWithMsg(c, err, "could not create schema", http.StatusInternalServerError)
		return
	}

	resp := CreateSchemaResponse{
		SchemaResponse: &SchemaResponse{
			ID:               createSchemaResponse.ID,
			Type:             createSchemaResponse.Type,
			Schema:           createSchemaResponse.Schema,
			CredentialSchema: createSchemaResponse.CredentialSchema,
		},
	}
	framework.Respond(c, resp, http.StatusCreated)
}

// GetSchema godoc
//
//	@Summary		Get a Credential Schema
//	@Description	Get a Credential Schema by its ID
//	@Tags			Schemas
//	@Accept			json
//	@Produce		json
//	@Param			id	path		string	true	"ID"
//	@Success		200	{object}	GetSchemaResponse
//	@Failure		400	{string}	string	"Bad request"
//	@Router			/v1/schemas/{id} [get]
func (sr SchemaRouter) GetSchema(c *gin.Context) {
	id := framework.GetParam(c, IDParam)
	if id == nil {
		errMsg := "cannot get schema without ID parameter"
		framework.LoggingRespondErrMsg(c, errMsg, http.StatusBadRequest)
		return
	}

	// TODO(gabe) differentiate between internal errors and not found schemas
	gotSchema, err := sr.service.GetSchema(c, schema.GetSchemaRequest{ID: *id})
	if err != nil {
		errMsg := fmt.Sprintf("could not get schema with id: %s", *id)
		framework.LoggingRespondErrWithMsg(c, err, errMsg, http.StatusInternalServerError)
		return
	}

	resp := GetSchemaResponse{
		SchemaResponse: &SchemaResponse{
			ID:               gotSchema.ID,
			Type:             gotSchema.Type,
			Schema:           gotSchema.Schema,
			CredentialSchema: gotSchema.CredentialSchema,
		},
	}
	framework.Respond(c, resp, http.StatusOK)
	return
}

type ListSchemasResponse struct {
	// Schemas is the list of all schemas the service holds
	Schemas []GetSchemaResponse `json:"schemas,omitempty"`

	// Pagination token to retrieve the next page of results. If the value is "", it means no further results for the request.
	NextPageToken string `json:"nextPageToken"`
}

// ListSchemas godoc
//
//	@Summary		List Credential Schemas
//	@Description	List Credential Schemas stored by the service
//	@Tags			Schemas
//	@Accept			json
//	@Produce		json
//	@Param			pageSize	query		number	false	"Hint to the server of the maximum elements to return. More may be returned. When not set, the server will return all elements."
//	@Param			pageToken	query		string	false	"Used to indicate to the server to return a specific page of the list results. Must match a previous requests' `nextPageToken`."
//	@Success		200			{object}	ListSchemasResponse
//	@Failure		500			{string}	string	"Internal server error"
//	@Router			/v1/schemas [get]
func (sr SchemaRouter) ListSchemas(c *gin.Context) {
	var pageRequest pagination.PageRequest
	if pagination.ParsePaginationQueryValues(c, &pageRequest) {
		return
	}

	gotSchemas, err := sr.service.ListSchemas(c, schema.ListSchemasRequest{
		PageRequest: &pageRequest,
	})
	if err != nil {
		errMsg := "could not list schemas"
		framework.LoggingRespondErrWithMsg(c, err, errMsg, http.StatusInternalServerError)
		return
	}

	schemas := make([]GetSchemaResponse, 0, len(gotSchemas.Schemas))
	for _, s := range gotSchemas.Schemas {
		schemas = append(schemas, GetSchemaResponse{
			SchemaResponse: &SchemaResponse{
				ID:               s.ID,
				Type:             s.Type,
				Schema:           s.Schema,
				CredentialSchema: s.CredentialSchema,
			},
		})
	}

	resp := ListSchemasResponse{Schemas: schemas}

	if pagination.MaybeSetNextPageToken(c, gotSchemas.NextPageToken, &resp.NextPageToken) {
		return
	}
	framework.Respond(c, resp, http.StatusOK)
}

type GetSchemaResponse struct {
	*SchemaResponse
}

// DeleteSchema godoc
//
//	@Summary		Delete a Credential Schema
//	@Description	Delete a Credential Schema by its ID
//	@Tags			Schemas
//	@Accept			json
//	@Produce		json
//	@Param			id	path		string	true	"ID"
//	@Success		204	{string}	string	"No Content"
//	@Failure		400	{string}	string	"Bad request"
//	@Failure		500	{string}	string	"Internal server error"
//	@Router			/v1/schemas/{id} [delete]
func (sr SchemaRouter) DeleteSchema(c *gin.Context) {
	id := framework.GetParam(c, IDParam)
	if id == nil {
		errMsg := "cannot delete a schema without an ID parameter"
		framework.LoggingRespondErrMsg(c, errMsg, http.StatusBadRequest)
		return
	}

	if err := sr.service.DeleteSchema(c, schema.DeleteSchemaRequest{ID: *id}); err != nil {
		errMsg := fmt.Sprintf("deleting schema with id: %s", *id)
		framework.LoggingRespondErrWithMsg(c, err, errMsg, http.StatusInternalServerError)
		return
	}

	framework.Respond(c, nil, http.StatusNoContent)
}
