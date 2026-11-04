package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestExplicitImageSizeAcceptsOnlyPhysicalSizes(t *testing.T) {
	cases := map[string]string{
		"864x1536":    "864x1536",
		" 1024X1024 ": "1024x1024",
		"":            "",
		"auto":        "",
		"AUTO":        "",
		"1024":        "",
		"0x1024":      "",
		"widexhigh":   "",
	}
	for input, want := range cases {
		if got := explicitImageSize(input); got != want {
			t.Errorf("explicitImageSize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestImageExactSizeForDefaultsToStrictPad(t *testing.T) {
	got := imageExactSizeFor("864x1536", "", "")
	if got.Size != "864x1536" || got.Fit != "pad" {
		t.Fatalf("default exact size = %#v, want 864x1536 pad", got)
	}
	if got = imageExactSizeFor("864x1536", "true", "COVER"); got.Fit != "cover" {
		t.Fatalf("cover fit = %#v", got)
	}
	if got = imageExactSizeFor("864x1536", "", "stretch"); got.Fit != "pad" {
		t.Fatalf("unknown fit must fall back to pad, got %#v", got)
	}
	for _, off := range []string{"false", "FALSE", "0", "off", "no"} {
		if got = imageExactSizeFor("864x1536", off, "cover"); got.Size != "" {
			t.Fatalf("strict_size=%q must disable exact sizing, got %#v", off, got)
		}
	}
	if got = imageExactSizeFor("", "", ""); got.Size != "" {
		t.Fatalf("no explicit size must not enable exact sizing, got %#v", got)
	}
}

func TestImageUpscalePlanWithExactSize(t *testing.T) {
	body := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2","size":"864x1536"}]}`)
	plan := imageUpscalePlanForRequest("gpt-image-2", body).withExactSize(imageExactSize{Size: "864x1536", Fit: "pad"})
	if !plan.enabled() || plan.Scale != "" || plan.RequestedSize != "864x1536" || plan.Fit != "pad" {
		t.Fatalf("plain model with explicit size plan = %#v", plan)
	}

	tier := imageUpscalePlanForRequest("gpt-image-2-2k", body).withExactSize(imageExactSize{})
	if tier.Scale != "2k" || tier.RequestedSize != "864x1536" {
		t.Fatalf("strict_size=false must keep the 2k tier plan, got %#v", tier)
	}

	cover := imageUpscalePlanForRequest("gpt-image-2-4k", body).withExactSize(imageExactSize{Size: "864x1536", Fit: "cover"})
	if cover.Scale != "4k" || cover.Fit != "cover" {
		t.Fatalf("tier plan must take the requested fit, got %#v", cover)
	}
}

func TestAddImageSizeDirectiveDescribesShape(t *testing.T) {
	cases := []struct {
		size        string
		orientation string
		ratio       string
	}{
		{"864x1536", "portrait", "9:16"},
		{"1024x1536", "portrait", "2:3"},
		{"1024x1024", "square", "1:1"},
		{"1536x1024", "landscape", "3:2"},
	}
	for _, test := range cases {
		body := buildImagesResponsesRequest("a red sneaker on white", []string{"data:image/png;base64,AAAA"}, []byte(`{"type":"image_generation","size":"`+test.size+`"}`))
		body = addImageSizeDirective(body, test.size)
		if got := gjson.GetBytes(body, "input.0.content.0.text").String(); got != "a red sneaker on white" {
			t.Fatalf("%s: user prompt changed to %q", test.size, got)
		}
		if got := gjson.GetBytes(body, "input.0.content.1.type").String(); got != "input_image" {
			t.Fatalf("%s: input image moved, content.1.type = %q", test.size, got)
		}
		directive := gjson.GetBytes(body, "input.0.content.2.text").String()
		for _, want := range []string{test.size, test.orientation, test.ratio} {
			if !strings.Contains(directive, want) {
				t.Fatalf("%s: directive %q does not mention %q", test.size, directive, want)
			}
		}
	}

	body := buildImagesResponsesRequest("a cat", nil, []byte(`{"type":"image_generation"}`))
	if got := addImageSizeDirective(body, ""); !bytes.Equal(got, body) {
		t.Fatalf("empty size must leave the body unchanged, got %s", got)
	}
}

// Reported: a 1024x1536 portrait request came back as 1312x1199 and a 1024x1024
// square as 1536x1024. The public API must return the requested canvas without
// cropping: the whole upstream image is kept and padded onto the exact size.
func TestCollectImagesResponseEnforcesExplicitSizeOnPlainModel(t *testing.T) {
	cases := []struct {
		upstreamWidth, upstreamHeight int
		size                          string
		wantWidth, wantHeight         int
	}{
		{1312, 1199, "864x1536", 864, 1536},
		{1536, 1024, "1024x1024", 1024, 1024},
		{1312, 1199, "1024x1536", 1024, 1536},
	}
	for _, test := range cases {
		upstream := imagesCompletedSSE(sizedPNGBase64(t, test.upstreamWidth, test.upstreamHeight), test.size)
		plan := imageUpscalePlan{RequestedSize: test.size, Fit: "pad"}
		out, _, _, info, err := collectImagesResponse(context.Background(), strings.NewReader(upstream), "b64_json", "gpt-image-2", nil, plan)
		if err != nil {
			t.Fatalf("%s: collectImagesResponse returned error: %v", test.size, err)
		}
		width, height := decodePNGSize(t, gjson.GetBytes(out, "data.0.b64_json").String())
		if width != test.wantWidth || height != test.wantHeight {
			t.Fatalf("%s: physical size = %dx%d, want %dx%d", test.size, width, height, test.wantWidth, test.wantHeight)
		}
		if info.Width != test.wantWidth || info.Height != test.wantHeight {
			t.Fatalf("%s: usage log size = %dx%d", test.size, info.Width, info.Height)
		}
	}
}

func mustDecodeBase64(t *testing.T, value string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func exactSizeUpstream(t *testing.T, width, height int) http.HandlerFunc {
	result := sizedPNGBase64(t, width, height)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"created_at":1710000000,"output":[{"type":"image_generation_call","result":"`+result+`","output_format":"png"}]}}`+"\n\n")
	}
}

func doExactSizeRequest(t *testing.T, fixture *imagesKeepaliveFixture, endpoint, contentType string, body []byte) []byte {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, fixture.server.URL+endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", contentType)
	response, err := fixture.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.StatusCode, payload)
	}
	fixture.assertReleased(t)
	return payload
}

func TestImagesEndpointsReturnExactExplicitSize(t *testing.T) {
	var multipartBody bytes.Buffer
	writer := multipart.NewWriter(&multipartBody)
	for key, value := range map[string]string{"model": "gpt-image-2", "prompt": "product card shot", "size": "864x1536"} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("image[]", "input.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(mustDecodeBase64(t, tinyPNGBase64)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, endpoint, contentType string
		body                        []byte
		wantWidth, wantHeight       int
	}{
		{"generations", "/v1/images/generations", "application/json", []byte(`{"model":"gpt-image-2","prompt":"product card shot","size":"864x1536"}`), 864, 1536},
		{"generations_cover", "/v1/images/generations", "application/json", []byte(`{"model":"gpt-image-2","prompt":"product card shot","size":"1024x1024","upscale_fit":"cover"}`), 1024, 1024},
		{"generations_opt_out", "/v1/images/generations", "application/json", []byte(`{"model":"gpt-image-2","prompt":"product card shot","size":"864x1536","strict_size":false}`), 1312, 1199},
		{"generations_auto", "/v1/images/generations", "application/json", []byte(`{"model":"gpt-image-2","prompt":"product card shot","size":"auto"}`), 1312, 1199},
		{"edits_json", "/v1/images/edits", "application/json", []byte(`{"model":"gpt-image-2","prompt":"product card shot","size":"864x1536","images":[{"image_url":"data:image/png;base64,` + tinyPNGBase64 + `"}]}`), 864, 1536},
		{"edits_multipart", "/v1/images/edits", writer.FormDataContentType(), multipartBody.Bytes(), 864, 1536},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newImagesKeepaliveFixture(t, exactSizeUpstream(t, 1312, 1199), database.ContinuousRetryPolicy{}, 0)
			payload := doExactSizeRequest(t, fixture, test.endpoint, test.contentType, test.body)
			width, height := decodePNGSize(t, gjson.GetBytes(payload, "data.0.b64_json").String())
			if width != test.wantWidth || height != test.wantHeight {
				t.Fatalf("physical size = %dx%d, want %dx%d", width, height, test.wantWidth, test.wantHeight)
			}
		})
	}
}
