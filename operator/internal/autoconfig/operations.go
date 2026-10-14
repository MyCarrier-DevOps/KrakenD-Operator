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

package autoconfig

// Operation identifies one OpenAPI operation: the method and path of its
// entry (the path after the URL transform), and the operationId and tags the
// spec declares for it.
type Operation struct {
	Method      string
	Path        string
	OperationID string
	Tags        []string
}

// OperationIssue is an operation the pipeline generates no endpoint for:
// Reason says why (a v1alpha1 reason constant), Message explains it.
type OperationIssue struct {
	Operation
	Reason  string
	Message string
	// methodKnown is set when Method came from a concrete method field or an
	// override, not from the entry's label.
	methodKnown bool
}
