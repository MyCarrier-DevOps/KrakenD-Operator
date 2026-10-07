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
	"fmt"
	"testing"

	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

func TestAdmissionMemo_KeepsTheMostRecentVerdicts(t *testing.T) {
	m := newAdmissionMemo()
	for i := range admissionMemoSize + 1 {
		m.Store(fmt.Sprint(i), configcheck.Verdict{Output: fmt.Sprint(i)})
	}

	if _, ok := m.Lookup("0"); ok {
		t.Error("the oldest verdict is kept past the memo's size")
	}
	if v, ok := m.Lookup(fmt.Sprint(admissionMemoSize)); !ok || v.Output != fmt.Sprint(admissionMemoSize) {
		t.Errorf("Lookup(newest) = %+v, %v; want the newest verdict", v, ok)
	}
}
