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
	"fmt"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// reviewRequester asks the API server whether the user who made the admission
// request in ctx may do attrs. The operator acts with its own grant, so where
// a write makes it act on the writer's behalf, the writer's own rights are
// checked. The review carries the user, UID, groups and extra attributes, so
// access that comes through a group or an authenticator is seen. It returns
// the requester's name for the denial. An error means there is no verdict.
func reviewRequester(
	ctx context.Context, c client.Client, attrs authorizationv1.ResourceAttributes,
) (user string, allowed bool, err error) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return "", false, fmt.Errorf("reviewing the requester's access: %w", err)
	}
	extra := make(map[string]authorizationv1.ExtraValue, len(req.UserInfo.Extra))
	for k, val := range req.UserInfo.Extra {
		extra[k] = authorizationv1.ExtraValue(val)
	}
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: req.UserInfo.Username, Groups: req.UserInfo.Groups, UID: req.UserInfo.UID, Extra: extra,
		ResourceAttributes: &attrs,
	}}
	if err := c.Create(ctx, sar); err != nil {
		return "", false, fmt.Errorf("reviewing the requester's access: %w", err)
	}
	return req.UserInfo.Username, sar.Status.Allowed, nil
}

// requireSecretRead is a Forbidden error on path unless the requester may get
// Secret name in namespace ns; use says what the operator does with it. An
// error means there is no verdict.
func requireSecretRead(
	ctx context.Context, c client.Client, path *field.Path, ns, name, use string,
) (field.ErrorList, error) {
	user, allowed, err := reviewRequester(ctx, c, authorizationv1.ResourceAttributes{
		Namespace: ns, Verb: "get", Resource: "secrets", Name: name})
	if err != nil || allowed {
		return nil, err
	}
	return field.ErrorList{field.Forbidden(path, fmt.Sprintf(
		"%q may not get Secret %s in namespace %s; %s", user, truncate(name, echoLimit), ns, use))}, nil
}
