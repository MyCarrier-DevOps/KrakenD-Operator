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

package telemetry

import "github.com/go-logr/logr"

// SwapGRPCTarget sets the logger grpc-go's records go to, nil for none, and
// returns the one it replaced, so a test can put it back.
func SwapGRPCTarget(target *logr.Logger) *logr.Logger {
	return grpcTarget.Swap(target)
}
