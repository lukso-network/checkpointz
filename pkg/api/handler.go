package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/attestantio/go-eth2-client/spec"
	"github.com/julienschmidt/httprouter"
	"github.com/samcm/checkpointz/pkg/beacon"
	"github.com/samcm/checkpointz/pkg/service/checkpointz"
	"github.com/samcm/checkpointz/pkg/service/eth"
	"github.com/sirupsen/logrus"
)

// Handler is an API handler that is responsible for negotiating with a HTTP api.
// All http-level concerns should be handled in this package, with the "namespaces" (eth/checkpointz)
// handling all business logic and dealing with concrete types.
type Handler struct {
	log logrus.FieldLogger

	eth         *eth.Handler
	checkpointz *checkpointz.Handler

	metrics Metrics
}

func NewHandler(log logrus.FieldLogger, beac beacon.FinalityProvider) *Handler {
	return &Handler{
		log: log.WithField("module", "api"),

		eth:         eth.NewHandler(log, beac, "checkpointz"),
		checkpointz: checkpointz.NewHandler(log, beac),

		metrics: NewMetrics("http"),
	}
}

func (h *Handler) Register(ctx context.Context, router *httprouter.Router) error {
	router.GET("/eth/v1/beacon/blocks/:block_id/root", h.wrappedHandler(h.handleEthV1BeaconBlocksRoot))
	router.GET("/eth/v1/beacon/states/:state_id/finality_checkpoints", h.wrappedHandler(h.handleEthV1BeaconStatesHeadFinalityCheckpoints))

	router.GET("/eth/v2/beacon/blocks/:block_id", h.wrappedHandler(h.handleEthV2BeaconBlocks))
	router.GET("/eth/v2/debug/beacon/states/:state_id", h.wrappedHandler(h.handleEthV2DebugBeaconStates))

	router.GET("/checkpointz/v1/status", h.wrappedHandler(h.handleCheckpointzStatus))
	router.GET("/checkpointz/v1/beacon/slots", h.wrappedHandler(h.handleCheckpointzBeaconSlots))
	router.GET("/checkpointz/v1/beacon/slots/:slot", h.wrappedHandler(h.handleCheckpointzBeaconSlot))

	return nil
}

func deriveRegisteredPath(request *http.Request, ps httprouter.Params) string {
	registeredPath := request.URL.Path
	for _, param := range ps {
		registeredPath = strings.Replace(registeredPath, param.Value, fmt.Sprintf(":%s", param.Key), 1)
	}

	return registeredPath
}

func (h *Handler) wrappedHandler(handler func(ctx context.Context, r *http.Request, p httprouter.Params, contentType ContentType) (*HTTPResponse, error)) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		start := time.Now()

		contentType := NewContentTypeFromRequest(r)
		ctx := r.Context()
		registeredPath := deriveRegisteredPath(r, p)

		h.log.WithFields(logrus.Fields{
			"method":       r.Method,
			"path":         r.URL.Path,
			"content_type": contentType,
			"accept":       r.Header.Get("Accept"),
		}).Debug("Handling request")

		h.metrics.ObserveRequest(r.Method, registeredPath)

		response := &HTTPResponse{}

		var err error

		defer func() {
			h.metrics.ObserveResponse(r.Method, registeredPath, fmt.Sprintf("%v", response.StatusCode), contentType.String(), time.Since(start))
		}()

		response, err = handler(ctx, r, p, contentType)
		if err != nil {
			if writeErr := WriteErrorResponse(w, err.Error(), response.StatusCode); writeErr != nil {
				h.log.WithError(writeErr).Error("Failed to write error response")
			}

			return
		}

		data, err := response.MarshalAs(contentType)
		if err != nil {
			if writeErr := WriteErrorResponse(w, err.Error(), http.StatusInternalServerError); writeErr != nil {
				h.log.WithError(writeErr).Error("Failed to write error response")
			}

			return
		}

		for header, value := range response.Headers {
			w.Header().Set(header, value)
		}

		if err := WriteContentAwareResponse(w, data, contentType); err != nil {
			h.log.WithError(err).Error("Failed to write response")
		}
	}
}

func (h *Handler) handleEthV2BeaconBlocks(ctx context.Context, r *http.Request, p httprouter.Params, contentType ContentType) (*HTTPResponse, error) {
	if err := ValidateContentType(contentType, []ContentType{ContentTypeJSON, ContentTypeSSZ}); err != nil {
		return NewUnsupportedMediaTypeResponse(nil), err
	}

	blockID, err := eth.NewBlockIdentifier(p.ByName("block_id"))
	if err != nil {
		return NewBadRequestResponse(nil), err
	}

	block, err := h.eth.BeaconBlock(ctx, blockID)
	if err != nil {
		return NewInternalServerErrorResponse(nil), err
	}

	var rsp = &HTTPResponse{}

	switch block.Version {
	case spec.DataVersionPhase0:
		rsp = NewSuccessResponse(ContentTypeResolvers{
			ContentTypeJSON: block.Phase0.MarshalJSON,
			ContentTypeSSZ:  block.Phase0.MarshalSSZ,
		})
	case spec.DataVersionAltair:
		rsp = NewSuccessResponse(ContentTypeResolvers{
			ContentTypeJSON: block.Altair.MarshalJSON,
			ContentTypeSSZ:  block.Altair.MarshalSSZ,
		})
	case spec.DataVersionBellatrix:
		rsp = NewSuccessResponse(ContentTypeResolvers{
			ContentTypeJSON: block.Bellatrix.MarshalJSON,
			ContentTypeSSZ:  block.Bellatrix.MarshalSSZ,
		})
	default:
		return NewInternalServerErrorResponse(nil), errors.New("unknown block version")
	}

	switch blockID.Type() {
	case eth.BlockIDRoot, eth.BlockIDGenesis, eth.BlockIDSlot:
		rsp.SetCacheControl("public, s-max-age=6000")
	case eth.BlockIDFinalized:
		// TODO(sam.calder-mason): This should be calculated using the Weak-Subjectivity period.
		rsp.SetCacheControl("public, s-max-age=30")
	case eth.BlockIDHead:
		rsp.SetCacheControl("public, s-max-age=30")
	}

	return rsp, nil
}

func (h *Handler) handleEthV2DebugBeaconStates(ctx context.Context, r *http.Request, p httprouter.Params, contentType ContentType) (*HTTPResponse, error) {
	if err := ValidateContentType(contentType, []ContentType{ContentTypeSSZ}); err != nil {
		return NewUnsupportedMediaTypeResponse(nil), err
	}

	id, err := eth.NewStateIdentifier(p.ByName("state_id"))
	if err != nil {
		return NewBadRequestResponse(nil), err
	}

	state, err := h.eth.BeaconState(ctx, id)
	if err != nil {
		return NewInternalServerErrorResponse(nil), err
	}

	if state == nil {
		return NewInternalServerErrorResponse(nil), errors.New("state not found")
	}

	rsp := NewSuccessResponse(ContentTypeResolvers{
		ContentTypeSSZ: func() ([]byte, error) {
			return *state, nil
		},
	})

	switch id.Type() {
	case eth.StateIDRoot, eth.StateIDGenesis, eth.StateIDSlot:
		// TODO(sam.calder-mason): This should be calculated using the Weak-Subjectivity period.
		rsp.SetCacheControl("public, s-max-age=6000")
	case eth.StateIDFinalized:
		// TODO(sam.calder-mason): This should be calculated using the Weak-Subjectivity period.
		rsp.SetCacheControl("public, s-max-age=180")
	case eth.StateIDHead:
		rsp.SetCacheControl("public, s-max-age=30")
	}

	return rsp, nil
}

func (h *Handler) handleCheckpointzStatus(ctx context.Context, r *http.Request, p httprouter.Params, contentType ContentType) (*HTTPResponse, error) {
	if err := ValidateContentType(contentType, []ContentType{ContentTypeJSON}); err != nil {
		return NewUnsupportedMediaTypeResponse(nil), err
	}

	status, err := h.checkpointz.V1Status(ctx, checkpointz.NewStatusRequest())
	if err != nil {
		return NewInternalServerErrorResponse(nil), err
	}

	return NewSuccessResponse(ContentTypeResolvers{
		ContentTypeJSON: func() ([]byte, error) {
			return json.Marshal(status)
		},
	}), nil
}

func (h *Handler) handleCheckpointzBeaconSlots(ctx context.Context, r *http.Request, p httprouter.Params, contentType ContentType) (*HTTPResponse, error) {
	if err := ValidateContentType(contentType, []ContentType{ContentTypeJSON}); err != nil {
		return NewUnsupportedMediaTypeResponse(nil), err
	}

	slots, err := h.checkpointz.V1BeaconSlots(ctx, checkpointz.NewBeaconSlotsRequest())
	if err != nil {
		return NewInternalServerErrorResponse(nil), err
	}

	return NewSuccessResponse(ContentTypeResolvers{
		ContentTypeJSON: func() ([]byte, error) {
			return json.Marshal(slots)
		},
	}), nil
}

func (h *Handler) handleCheckpointzBeaconSlot(ctx context.Context, r *http.Request, p httprouter.Params, contentType ContentType) (*HTTPResponse, error) {
	if err := ValidateContentType(contentType, []ContentType{ContentTypeJSON}); err != nil {
		return NewUnsupportedMediaTypeResponse(nil), err
	}

	slot, err := eth.NewSlotFromString(p.ByName("slot"))
	if err != nil {
		return NewBadRequestResponse(nil), err
	}

	slots, err := h.checkpointz.V1BeaconSlot(ctx, checkpointz.NewBeaconSlotRequest(slot))
	if err != nil {
		return NewInternalServerErrorResponse(nil), err
	}

	return NewSuccessResponse(ContentTypeResolvers{
		ContentTypeJSON: func() ([]byte, error) {
			return json.Marshal(slots)
		},
	}), nil
}

func (h *Handler) handleEthV1BeaconStatesHeadFinalityCheckpoints(ctx context.Context, r *http.Request, p httprouter.Params, contentType ContentType) (*HTTPResponse, error) {
	if err := ValidateContentType(contentType, []ContentType{ContentTypeJSON}); err != nil {
		return NewUnsupportedMediaTypeResponse(nil), err
	}

	id, err := eth.NewStateIdentifier(p.ByName("state_id"))
	if err != nil {
		return NewBadRequestResponse(nil), err
	}

	finality, err := h.eth.FinalityCheckpoints(ctx, id)
	if err != nil {
		return NewInternalServerErrorResponse(nil), err
	}

	return NewSuccessResponse(ContentTypeResolvers{
		ContentTypeJSON: func() ([]byte, error) {
			return json.Marshal(finality)
		},
	}), nil
}

func (h *Handler) handleEthV1BeaconBlocksRoot(ctx context.Context, r *http.Request, p httprouter.Params, contentType ContentType) (*HTTPResponse, error) {
	if err := ValidateContentType(contentType, []ContentType{ContentTypeJSON}); err != nil {
		return NewUnsupportedMediaTypeResponse(nil), err
	}

	id, err := eth.NewBlockIdentifier(p.ByName("block_id"))
	if err != nil {
		return NewBadRequestResponse(nil), err
	}

	root, err := h.eth.BlockRoot(ctx, id)
	if err != nil {
		return NewInternalServerErrorResponse(nil), err
	}

	wrapped := struct {
		Root string `json:"root"`
	}{
		Root: fmt.Sprintf("%x", root),
	}

	return NewSuccessResponse(ContentTypeResolvers{
		ContentTypeJSON: func() ([]byte, error) {
			return json.Marshal(wrapped)
		},
	}), nil
}