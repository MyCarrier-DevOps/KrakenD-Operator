package autoconfig

import (
	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// BuildAdditionalEntries synthesizes user-designated AdditionalEndpoint specs
// into full EndpointEntry values. defaultHost is the backend host derived from
// spec.openapi.url. When an entry's InheritDefaults is true, spec.defaults are
// applied fill-only (the entry's explicit fields win).
func BuildAdditionalEntries(
	specs []v1alpha1.AdditionalEndpoint,
	defaults *v1alpha1.Defaults,
	defaultHost string,
) []v1alpha1.EndpointEntry {
	if len(specs) == 0 {
		return nil
	}
	entries := make([]v1alpha1.EndpointEntry, 0, len(specs))
	for _, s := range specs {
		method := s.Method
		if method == "" {
			method = "GET"
		}

		entry := v1alpha1.EndpointEntry{
			Endpoint:          s.Endpoint,
			Method:            method,
			Timeout:           s.Timeout,
			CacheTTL:          s.CacheTTL,
			InputHeaders:      s.InputHeaders,
			InputQueryStrings: s.InputQueryStrings,
			OutputEncoding:    s.OutputEncoding,
			ConcurrentCalls:   s.ConcurrentCalls,
			ExtraConfig:       s.ExtraConfig,
		}

		if len(s.Backends) > 0 {
			entry.Backends = s.Backends
		} else {
			host := s.Host
			if host == "" {
				host = defaultHost
			}
			urlPattern := s.BackendURLPattern
			if urlPattern == "" {
				urlPattern = s.Endpoint
			}
			entry.Backends = []v1alpha1.BackendSpec{{
				Host:       []string{host},
				URLPattern: urlPattern,
				Method:     method,
				Encoding:   s.Encoding,
			}}
			if s.Encoding == "no-op" && entry.OutputEncoding == "" {
				entry.OutputEncoding = "no-op"
			}
		}

		if s.InheritDefaults != nil && *s.InheritDefaults && defaults != nil {
			inheritDefaults(&entry, defaults)
		}

		entries = append(entries, entry)
	}
	return entries
}

// inheritDefaults fills unset endpoint/backend fields from spec.defaults.
// Explicit entry fields are preserved; ExtraConfig is deep-merged with the
// entry's own values winning on key collisions.
func inheritDefaults(entry *v1alpha1.EndpointEntry, defaults *v1alpha1.Defaults) {
	if e := defaults.Endpoint; e != nil {
		if entry.Timeout == nil {
			entry.Timeout = e.Timeout
		}
		if entry.CacheTTL == nil {
			entry.CacheTTL = e.CacheTTL
		}
		if entry.OutputEncoding == "" {
			entry.OutputEncoding = e.OutputEncoding
		}
		if entry.ConcurrentCalls == nil {
			entry.ConcurrentCalls = e.ConcurrentCalls
		}
		if entry.InputHeaders == nil {
			entry.InputHeaders = e.InputHeaders
		}
		if entry.InputQueryStrings == nil {
			entry.InputQueryStrings = e.InputQueryStrings
		}
		if e.ExtraConfig != nil {
			// base = default, override = entry → entry wins on overlap.
			entry.ExtraConfig = mergeExtraConfig(e.ExtraConfig, entry.ExtraConfig)
		}
	}
	if defaults.Backend != nil {
		for i := range entry.Backends {
			applyBackendDefaultsToBackend(&entry.Backends[i], defaults.Backend)
		}
	}
	if defaults.PolicyRef != nil {
		for i := range entry.Backends {
			if entry.Backends[i].PolicyRef == nil {
				entry.Backends[i].PolicyRef = defaults.PolicyRef
			}
		}
	}
}
