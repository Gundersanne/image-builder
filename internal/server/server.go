//go:generate go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen --package=server --generate types,server,spec -o api.go api.yaml
package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/osbuild/image-builder/internal/cloudapi"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
)

type Server struct {
	logger   *logrus.Logger
	echo     *echo.Echo
	client   cloudapi.OsbuildClient
	aws      AWSConfig
	gcp      GCPConfig
	azure    AzureConfig
	orgIds   []string
	distsDir string
}

type AWSConfig struct {
	Region          string
	AccessKeyId     string
	SecretAccessKey string
	S3Bucket        string
}

type GCPConfig struct {
	Region string
	Bucket string
}

type AzureConfig struct {
	Location string
}

type Handlers struct {
	server *Server
}

func NewServer(logger *logrus.Logger, client cloudapi.OsbuildClient, awsConfig AWSConfig, gcpConfig GCPConfig, azureConfig AzureConfig, orgIds []string, distsDir string) *Server {
	spec, err := GetSwagger()
	if err != nil {
		panic(err)
	}
	majorVersion := strings.Split(spec.Info.Version, ".")[0]

	s := Server{
		logger,
		echo.New(),
		client,
		awsConfig,
		gcpConfig,
		azureConfig,
		orgIds,
		distsDir,
	}
	var h Handlers
	h.server = &s
	s.echo.Binder = binder{}
	s.echo.HTTPErrorHandler = s.HTTPErrorHandler
	RegisterHandlers(s.echo.Group(fmt.Sprintf("%s/v%s", RoutePrefix(), majorVersion), s.VerifyIdentityHeader), &h)
	RegisterHandlers(s.echo.Group(fmt.Sprintf("%s/v%s", RoutePrefix(), spec.Info.Version), s.VerifyIdentityHeader), &h)

	/* Used for the livenessProbe */
	s.echo.GET("/status", func(c echo.Context) error {
		return h.GetVersion(c)
	})

	/* Used for the readinessProbe */
	s.echo.GET("/ready", func(c echo.Context) error {
		// make sure distributions are available
		distributions, err := AvailableDistributions(s.distsDir)
		if err != nil {
			return err
		}
		if len(distributions) == 0 {
			return echo.NewHTTPError(http.StatusInternalServerError, "no distributions defined")
		}

		resp, err := s.client.Version()
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("Failed to contact osbuild-composer: %s", body))
		}

		ready := map[string]string{
			"readiness": "ready",
		}

		return c.JSON(http.StatusOK, ready)
	})

	return &s
}

func (s *Server) Run(address string) {
	s.logger.Infof("🚀 Starting image-builder server on %v ...\n", address)
	err := s.echo.Start(address)
	if err != nil {
		s.logger.Errorln(fmt.Sprintf("Error starting echo server: %v", err))
	}
}

func (h *Handlers) GetVersion(ctx echo.Context) error {
	spec, err := GetSwagger()
	if err != nil {
		return err
	}
	version := Version{spec.Info.Version}
	return ctx.JSON(http.StatusOK, version)
}

func (h *Handlers) GetOpenapiJson(ctx echo.Context) error {
	spec, err := GetSwagger()
	if err != nil {
		return err
	}
	spec.AddServer(&openapi3.Server{URL: fmt.Sprintf("%s/v%s", RoutePrefix(), spec.Info.Version)})
	return ctx.JSON(http.StatusOK, spec)
}

func (h *Handlers) GetDistributions(ctx echo.Context) error {
	distributions, err := AvailableDistributions(h.server.distsDir)
	if err != nil {
		return err
	}
	return ctx.JSON(http.StatusOK, distributions)
}

func (h *Handlers) GetArchitectures(ctx echo.Context, distribution string) error {
	archs, err := ArchitecturesForImage(h.server.distsDir, distribution)
	if err != nil {
		return err
	}
	return ctx.JSON(http.StatusOK, archs)
}

func (h *Handlers) GetComposeStatus(ctx echo.Context, composeId string) error {
	resp, err := h.server.client.ComposeStatus(composeId)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("%s", body))
	}

	var cloudStat cloudapi.ComposeStatus
	err = json.NewDecoder(resp.Body).Decode(&cloudStat)
	if err != nil {
		return err
	}

	status := ComposeStatus{
		ImageStatus: ImageStatus{
			Status:       cloudStat.ImageStatus.Status,
			UploadStatus: nil,
		},
	}

	if cloudStat.ImageStatus.UploadStatus != nil {
		status.ImageStatus.UploadStatus = &UploadStatus{
			Status: cloudStat.ImageStatus.UploadStatus.Status,
			Type:   UploadTypes(cloudStat.ImageStatus.UploadStatus.Type),
		}
	}

	return ctx.JSON(http.StatusOK, status)
}

func (h *Handlers) ComposeImage(ctx echo.Context) error {
	var composeRequest ComposeRequest
	err := ctx.Bind(&composeRequest)
	if err != nil {
		return err
	}

	if len(composeRequest.ImageRequests) != 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Exactly one image request should be included")
	}

	if len(composeRequest.ImageRequests[0].UploadRequests) != 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Exactly one upload request should be included")
	}

	repositories, err := RepositoriesForImage(h.server.distsDir, composeRequest.Distribution, composeRequest.ImageRequests[0].Architecture)
	if err != nil {
		return err
	}

	uploadReq, err := h.server.buildUploadRequest(composeRequest.ImageRequests[0].UploadRequests[0])
	if err != nil {
		return err
	}

	custom, err := buildCustomizations(composeRequest.Customizations)
	if err != nil {
		return err
	}

	cloudCR := cloudapi.ComposeRequest{
		Distribution:   composeRequest.Distribution,
		Customizations: custom,
		ImageRequests: []cloudapi.ImageRequest{
			{
				Architecture: composeRequest.ImageRequests[0].Architecture,
				ImageType:    composeRequest.ImageRequests[0].ImageType,
				Repositories: repositories,
				UploadRequests: []cloudapi.UploadRequest{
					uploadReq,
				},
			},
		},
	}

	resp, err := h.server.client.Compose(cloudCR)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return echo.NewHTTPError(resp.StatusCode, "Failed posting compose request to osbuild-composer")
		}
		return echo.NewHTTPError(resp.StatusCode, fmt.Sprintf("Failed posting compose request to osbuild-composer: %s", body))
	}

	var composeResult cloudapi.ComposeResult
	err = json.NewDecoder(resp.Body).Decode(&composeResult)
	if err != nil {
		return err
	}
	return ctx.JSON(http.StatusCreated, ComposeResponse{
		Id: composeResult.Id,
	})
}

func (s *Server) buildUploadRequest(ur UploadRequest) (cloudapi.UploadRequest, error) {
	// HACK deepmap doesn't really support `oneOf`, so marshal and unmarshal into target object
	optionsJSON, err := json.Marshal(ur.Options)
	if err != nil {
		return cloudapi.UploadRequest{}, echo.NewHTTPError(http.StatusBadRequest, "Unable to marshal UploadRequestOptions")
	}
	switch ur.Type {
	case "aws":
		var awsOptions AWSUploadRequestOptions
		err = json.Unmarshal(optionsJSON, &awsOptions)
		if err != nil {
			return cloudapi.UploadRequest{}, echo.NewHTTPError(http.StatusBadRequest, "Unable to unmarshal UploadRequestOptions")
		}
		return cloudapi.UploadRequest{
			Type: cloudapi.UploadTypes(ur.Type),
			Options: cloudapi.AWSUploadRequestOptions{
				Ec2: cloudapi.AWSUploadRequestOptionsEc2{
					AccessKeyId:       s.aws.AccessKeyId,
					SecretAccessKey:   s.aws.SecretAccessKey,
					ShareWithAccounts: &awsOptions.ShareWithAccounts,
				},
				S3: cloudapi.AWSUploadRequestOptionsS3{
					AccessKeyId:     s.aws.AccessKeyId,
					SecretAccessKey: s.aws.SecretAccessKey,
					Bucket:          s.aws.S3Bucket,
				},
				Region: s.aws.Region,
			},
		}, nil
	case "gcp":
		var gcpOptions GCPUploadRequestOptions
		err = json.Unmarshal(optionsJSON, &gcpOptions)
		if err != nil {
			return cloudapi.UploadRequest{}, echo.NewHTTPError(http.StatusBadRequest, "Unable to unmarshal into GCPUploadRequestOptions")
		}
		return cloudapi.UploadRequest{
			Type: cloudapi.UploadTypes(ur.Type),
			Options: cloudapi.GCPUploadRequestOptions{
				Bucket:            s.gcp.Bucket,
				Region:            &s.gcp.Region,
				ShareWithAccounts: &gcpOptions.ShareWithAccounts,
			},
		}, nil
	case "azure":
		var azureOptions AzureUploadRequestOptions
		err = json.Unmarshal(optionsJSON, &azureOptions)
		if err != nil {
			return cloudapi.UploadRequest{}, echo.NewHTTPError(http.StatusBadRequest, "Unable to unmarshal into AzureUploadRequestOptions")
		}
		return cloudapi.UploadRequest{
			Type: cloudapi.UploadTypes(ur.Type),
			Options: cloudapi.AzureUploadRequestOptions{
				TenantId:       azureOptions.TenantId,
				SubscriptionId: azureOptions.SubscriptionId,
				ResourceGroup:  azureOptions.ResourceGroup,
				Location:       s.azure.Location,
			},
		}, nil
	default:
		return cloudapi.UploadRequest{}, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Unknown UploadRequest type %s", ur.Type))
	}
}

func buildCustomizations(cust *Customizations) (*cloudapi.Customizations, error) {
	if cust == nil {
		return nil, nil
	}

	res := &cloudapi.Customizations{}
	if cust.Subscription != nil {
		res.Subscription = &cloudapi.Subscription{
			ActivationKey: cust.Subscription.ActivationKey,
			BaseUrl:       cust.Subscription.BaseUrl,
			Insights:      cust.Subscription.Insights,
			Organization:  cust.Subscription.Organization,
			ServerUrl:     cust.Subscription.ServerUrl,
		}
	}

	if cust.Packages != nil {
		res.Packages = cust.Packages
	}

	return res, nil
}

func (h *Handlers) GetPackages(ctx echo.Context, params GetPackagesParams) error {
	packages, err := FindPackages(h.server.distsDir, params.Distribution, params.Architecture, params.Search)
	if err != nil {
		return err
	}

	limit := 100
	if params.Limit != nil {
		if *params.Limit > 0 {
			limit = *params.Limit
		}
	}

	offset := 0
	if params.Offset != nil {
		if *params.Offset >= len(packages) {
			offset = len(packages) - 1
		} else if *params.Offset > 0 {
			offset = *params.Offset
		}
	}

	upto := offset + limit
	if upto > len(packages) {
		upto = len(packages)
	}

	spec, err := GetSwagger()
	if err != nil {
		return err
	}

	return ctx.JSON(http.StatusOK, PackagesResponse{
		Meta: struct {
			Count int `json:"count"`
		}{
			len(packages[offset:upto]),
		},
		Links: struct {
			First string `json:"first"`
			Last  string `json:"last"`
		}{
			fmt.Sprintf("%v/v%v/packages?search=%v&distribution=%v&architecture=%v&offset=%v&limit=%v",
				RoutePrefix(), spec.Info.Version, params.Search, params.Distribution, params.Architecture, offset, limit),
			fmt.Sprintf("%v/v%v/packages?search=%v&distribution=%v&architecture=%v&offset=%v&limit=%v",
				RoutePrefix(), spec.Info.Version, params.Search, params.Distribution, params.Architecture, len(packages)-1, limit),
		},
		Data: packages[offset:upto],
	})
}

func RoutePrefix() string {
	pathPrefix, ok := os.LookupEnv("PATH_PREFIX")
	if !ok {
		pathPrefix = "api"
	}
	appName, ok := os.LookupEnv("APP_NAME")
	if !ok {
		appName = "image-builder"
	}
	return fmt.Sprintf("/%s/%s", pathPrefix, appName)
}

// A simple echo.Binder(), which only accepts application/json, but is more
// strict than echo's DefaultBinder. It does not handle binding query
// parameters either.
type binder struct{}

func (b binder) Bind(i interface{}, ctx echo.Context) error {
	request := ctx.Request()

	contentType := request.Header["Content-Type"]
	if len(contentType) != 1 || contentType[0] != "application/json" {
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, "request must be json-encoded")
	}

	err := json.NewDecoder(request.Body).Decode(i)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("cannot parse request body: %v", err))
	}

	return nil
}

type IdentityHeader struct {
	Identity struct {
		Internal struct {
			OrgId string `json:"org_id"`
		} `json:"internal"`
	} `json:"identity"`
}

func orgIdAllowed(header IdentityHeader, orgIds []string) bool {
	for _, org := range orgIds {
		if org == "*" {
			return true
		}
		if header.Identity.Internal.OrgId == org {
			return true
		}
	}
	return false
}

// clouddot guidelines requires 404 instead of 403 when unauthorized
func (s *Server) VerifyIdentityHeader(nextHandler echo.HandlerFunc) echo.HandlerFunc {
	return func(ctx echo.Context) error {
		request := ctx.Request()

		idHeaderB64 := request.Header["X-Rh-Identity"]
		if len(idHeaderB64) != 1 {
			return echo.NewHTTPError(http.StatusNotFound, "Auth header is not present")
		}

		b64Result, err := base64.StdEncoding.DecodeString(idHeaderB64[0])
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "Auth header has incorrect format")
		}

		var idHeader IdentityHeader
		err = json.Unmarshal([]byte(strings.TrimSuffix(fmt.Sprintf("%s", b64Result), "\n")), &idHeader)
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "Auth header has incorrect format")
		}

		s.logger.Infof("Verify identity for user with internal org_id '%v' in header '%v' \n", idHeader.Identity.Internal.OrgId, idHeaderB64[0])

		if !orgIdAllowed(idHeader, s.orgIds) {
			return echo.NewHTTPError(http.StatusNotFound, "Organization not allowed")
		}

		return nextHandler(ctx)
	}
}

func (s *Server) HTTPErrorHandler(err error, c echo.Context) {
	var errors []HTTPError
	he, ok := err.(*echo.HTTPError)
	if ok {
		if he.Internal != nil {
			if herr, ok := he.Internal.(*echo.HTTPError); ok {
				he = herr
			}
		}
	} else {
		he = &echo.HTTPError{
			Code:    http.StatusInternalServerError,
			Message: http.StatusText(http.StatusInternalServerError),
		}
	}

	// Only log internal errors
	if he.Code == http.StatusInternalServerError {
		s.logger.Errorln(fmt.Sprintf("Internal error %v: %v, %v", he.Code, he.Message, err))

	}

	errors = append(errors, HTTPError{
		Title:  strconv.Itoa(he.Code),
		Detail: fmt.Sprintf("%v", he.Message),
	})

	// Send response
	if !c.Response().Committed {
		if c.Request().Method == http.MethodHead {
			err = c.NoContent(he.Code)
		} else {
			err = c.JSON(he.Code, &HTTPErrorList{
				errors,
			})
		}
		if err != nil {
			s.logger.Errorln(err)
		}
	}
}
