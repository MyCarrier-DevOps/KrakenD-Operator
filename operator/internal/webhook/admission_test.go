/*
Copyright 2026 The KrakenD Operator Authors.

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

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// review sends obj (and old, on an update) through the admission handler the
// manager registers for v, as username. The handler decodes into an empty
// prototype, as the manager's does, so the old object is never decoded on top
// of the new one.
func review(t *testing.T, v admission.CustomValidator, username string, obj, old runtime.Object) admission.Response {
	t.Helper()
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "uid", Operation: admissionv1.Create, UserInfo: authenticationv1.UserInfo{Username: username},
	}}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	req.Object = runtime.RawExtension{Raw: raw}
	if old != nil {
		oldRaw, err := json.Marshal(old)
		if err != nil {
			t.Fatal(err)
		}
		req.Operation, req.OldObject = admissionv1.Update, runtime.RawExtension{Raw: oldRaw}
	}
	proto := reflect.New(reflect.TypeOf(obj).Elem()).Interface().(runtime.Object)
	return admission.WithCustomValidator(testScheme(), proto, v).Handle(context.Background(), req)
}

func TestAdmission_FieldErrorsAre422WithCauses(t *testing.T) {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "missing"},
			Endpoints: []v1alpha1.EndpointEntry{{Endpoint: "/a", Method: "GET",
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}}}}},
	}
	resp := review(t, &EndpointValidator{Client: fakeClient()}, "alice", ep, nil)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want 422", resp.Result)
	}
	if c := resp.Result.Details.Causes; len(c) != 1 || c[0].Field != "spec.gatewayRef.name" ||
		c[0].Type != metav1.CauseTypeFieldValueNotFound {
		t.Errorf("causes = %+v, want NotFound on spec.gatewayRef.name", c)
	}
}

func TestAdmission_LookupFailuresAre500(t *testing.T) {
	c := fakeClientBuilderWith(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("cache not synced")
		},
	})
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "default"},
		Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "gw"}},
	}
	resp := review(t, &EndpointValidator{Client: c}, "alice", ep, nil)
	if resp.Allowed || resp.Result.Code != http.StatusInternalServerError {
		t.Errorf("response = %+v, want 500", resp.Result)
	}
}

func TestAdmission_AutoConfigGatewayLookupFailureIs500(t *testing.T) {
	c := fakeClientBuilderWith(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("cache not synced")
		},
	})
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
		Spec:       v1alpha1.KrakenDAutoConfigSpec{GatewayRef: v1alpha1.GatewayRef{Name: "gw"}},
	}
	resp := review(t, &AutoConfigValidator{Client: c}, "alice", ac, nil)
	if resp.Allowed || resp.Result.Code != http.StatusInternalServerError {
		t.Errorf("response = %+v, want 500", resp.Result)
	}
}

func TestAdmission_EndpointConflictListFailureIs500(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"}}
	c := fakeClientBuilderWith(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	}, gw)
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "default"},
		Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "gw"}},
	}
	resp := review(t, &EndpointValidator{Client: c}, "alice", ep, nil)
	if resp.Allowed || resp.Result.Code != http.StatusInternalServerError {
		t.Errorf("response = %+v, want 500", resp.Result)
	}
}

func TestNewErrors_KeepsOnlyErrorsTheOldObjectLacked(t *testing.T) {
	p := field.NewPath("spec", "x")
	stored := field.ErrorList{field.Invalid(p, "a", "bad"), field.Required(p.Child("y"), "")}
	current := field.ErrorList{
		field.Invalid(p, "a", "bad"),       // unchanged
		field.Invalid(p, "b", "bad"),       // same field and type, a different value
		field.Required(p.Child("y"), ""),   // unchanged
		field.Required(p.Child("z"), ""),   // a new field
		field.Forbidden(p.Child("y"), "n"), // a new type on a known field
	}

	got := newErrors(current, stored)

	want := []string{current[1].Error(), current[3].Error(), current[4].Error()}
	var have []string
	for _, e := range got {
		have = append(have, e.Error())
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("newErrors = %q, want %q", have, want)
	}
}
