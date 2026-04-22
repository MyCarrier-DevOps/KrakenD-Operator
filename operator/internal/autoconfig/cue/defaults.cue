// Default CUE definitions for the KrakenD Operator autoconfig pipeline.
//
// This provides the transformation logic to produce KrakenDEndpoint
// EndpointEntry objects from raw openapi JSON endpoints.
//
// Inputs (injected by the operator):
//   _spec — the fetched OpenAPI spec (JSON), structured as a single-service
//           OpenAPI document with paths, parameters, requestBody, responses
//   _env  — deployment environment string (e.g. "dev", "preprod", "prod")
//
// Output:
//   endpoint — struct keyed by "path:METHOD", each value an EndpointEntry

import (
	"strings"
	"encoding/json"
	"encoding/base64"
	"list"
)

// _spec receives the fetched OpenAPI spec data.
_spec: _

// _env selects the deployment environment. Custom CUE definitions can
// branch on this value for per-environment host resolution, matching
// the #internalHost.dev/preprod/prod pattern from KrakenD-SwaggerParse.
_env: string | *"dev"

// _httpMethods lists recognized OpenAPI HTTP methods.
// Path-item keys not in this set (parameters, summary, etc.) are skipped.
_httpMethods: ["get", "post", "put", "delete", "patch", "head", "options", "trace"]

// _defaultHost is the backend host URL. The operator injects this value
// from the host derived from spec.openapi.url, which is the location used
// to fetch the OpenAPI document. Custom CUE definitions can override it
// for per-environment resolution.
_defaultHost: string | *"http://localhost"

// _defaultTimeout is the endpoint timeout. Per-operation overrides can set
// endpoint_def.timeout in the spec data (matching SwaggerParse behavior).
_defaultTimeout: "3s"

// _defaultAuth controls whether Authorization and X-MC-Api-Key headers are
// forwarded by default. Per-operation spec data can override via
// endpoint_def.authorization.
_defaultAuth: true

// _defaultRateLimit holds default rate limit configuration for router-level
// rate limiting, matching KrakenD-SwaggerParse's default values.
_defaultRateLimit: {
	every:    "2s"
	max_rate: 10
	capacity: 0
	strategy: "header"
}

// endpoint is the output label consumed by the operator. Each key is
// "path:METHOD" and each value conforms to the EndpointEntry schema.
endpoint: {
	if _spec.paths != _|_ {
		for _path, _pathItem in _spec.paths {
			for _verb, _op in _pathItem {
				if list.Contains(_httpMethods, _verb) {
					// Support path rewrite: if the operation defines a rewrite
					// field, use it as the endpoint path (SwaggerParse pattern).
					let _endpointPath = *_op.rewrite | _path
					let _timeout = *_op.timeout | _defaultTimeout
					let _auth = *_op.authorization | _defaultAuth
					let _rateLimit = *_op.api_rate_limit | _defaultRateLimit

					"\(_endpointPath):\(strings.ToUpper(_verb))": {
						endpoint: _endpointPath
						method:   strings.ToUpper(_verb)

						// no-op output encoding (pass-through proxy)
						if _op.no_op != _|_ {
							outputEncoding: "no-op"
						}

						// Timeout as a Go duration string
						timeout: _timeout

						backends: [{
							host:       [_defaultHost]
							urlPattern: _path
							method:     strings.ToUpper(_verb)
						}]

						// Input headers: conditionally include auth headers and
						// always include Content-Type, plus any declared header
						// parameters from the operation and path item.
						inputHeaders: [
							for _h in [
								{add: _auth, val: "Authorization"},
								{add: _auth, val: "X-MC-Api-Key"},
								{add: true, val:  "Content-Type"},
							] if _h.add {
								_h.val
							},
							if _op.parameters != _|_
							for _p in _op.parameters
							if _p["in"] == "header" {
								_p.name
							},
							if _pathItem.parameters != _|_
							for _p in _pathItem.parameters
							if _p["in"] == "header" {
								_p.name
							},
						]

						// Query string parameters from operation and path item.
						inputQueryStrings: [
							if _op.parameters != _|_
							for _p in _op.parameters
							if _p["in"] == "query" {
								_p.name
							},
							if _pathItem.parameters != _|_
							for _p in _pathItem.parameters
							if _p["in"] == "query" {
								_p.name
							},
						]

						// Extra config: rate limiting + OpenAPI documentation,
						// matching KrakenD-SwaggerParse's extra_config structure.
						extraConfig: {
							"qos/ratelimit/router": {
								every:    [if _rateLimit.every != _|_ {_rateLimit.every}, "2s"][0]
								max_rate: [if _rateLimit.max_rate != _|_ {_rateLimit.max_rate}, 10][0]
								if _rateLimit.capacity != _|_ {
									capacity: _rateLimit.capacity
								}
								if _rateLimit.client_max_rate != _|_ {
									client_max_rate: _rateLimit.client_max_rate
								}
								if _rateLimit.client_capacity != _|_ {
									client_capacity: _rateLimit.client_capacity
								}
								if _auth {
									strategy: _rateLimit.strategy | "header"
									key:      *_rateLimit.key | "Authorization"
								}
							}
							"documentation/openapi": {
								audience: *_op.audience | ["public"]
								if _op.description != _|_ {
									description: *_op.description | ""
								}
								if _op.summary != _|_ {
									summary: *_op.summary | ""
								}
								if _op.groups != _|_ {
									tags: *_op.groups | []
								}
								if _op.tags != _|_ && _op.groups == _|_ {
									tags: *_op.tags | []
								}
								if _op.operationId != _|_ {
									operation_id: *_op.operationId | ""
								}
								if _op.parameters != _|_ {
									query_definition: [for _p in _op.parameters if _p["in"] == "query" {
										if _p.description != _|_ {
											description: _p.description
										}
										name: _p.name
										if _p.required != _|_ {
											required: _p.required
										}
										if _p.schema != _|_ {
											if _p.schema.type != _|_ {
												type: _p.schema.type
											}
											if _p.schema.format != _|_ {
												format: _p.schema.format
											}
											if _p.schema.enum != _|_ {
												enum: _p.schema.enum
											}
										}
										if _p.example != _|_ {
											example: _p.example
										}
										if _p.examples != _|_ {
											examples: _p.examples
										}
									}]
									param_definition: [for _p in _op.parameters if _p["in"] == "path" {
										if _p.description != _|_ {
											description: _p.description
										}
										required: true
										name:     _p.name
										if _p.schema != _|_ {
											if _p.schema.type != _|_ {
												type: _p.schema.type
											}
											if _p.schema.format != _|_ {
												format: _p.schema.format
											}
											if _p.schema.enum != _|_ {
												enum: _p.schema.enum
											}
										}
										if _p.example != _|_ {
											example: _p.example
										}
										if _p.examples != _|_ {
											examples: _p.examples
										}
									}]
									header_definition: [for _p in _op.parameters if _p["in"] == "header" {
										if _p.description != _|_ {
											description: _p.description
										}
										name: _p.name
										if _p.required != _|_ {
											required: _p.required
										}
										if _p.schema != _|_ {
											if _p.schema.type != _|_ {
												type: _p.schema.type
											}
											if _p.schema.enum != _|_ {
												enum: _p.schema.enum
											}
										}
									}]
								}
								if _op.requestBody.content != _|_ {
									request_definition: {
										[for _ctKey, _ctVal in _op.requestBody.content {
											content_type: _ctKey
											if _op.requestBody.description != _|_ {
												description: _op.requestBody.description
											}
											if _ctVal.schema.$ref != _|_ {
												let _parts = strings.Split(_ctVal.schema.$ref, "/")
												ref: strings.ToLower(_parts[len(_parts)-1])
											}
											if _ctVal.schema.$ref == _|_ && _ctVal.schema.allOf != _|_ {
												let _modifiedSchema = {
													allOf: [for _item in _ctVal.schema.allOf {
														if _item.$ref != _|_ {
															let _parts = strings.Split(_item.$ref, "/")
															$ref: "#/components/schemas/\(strings.ToLower(_parts[len(_parts)-1]))"
														}
														if _item.$ref == _|_ {
															_item
														}
													}]
													if _ctVal.schema.description != _|_ {
														description: _ctVal.schema.description
													}
													for _k, _v in _ctVal.schema if _k != "allOf" && _k != "description" {
														"\(_k)": _v
													}
												}
												example_schema: base64.Encode(null, json.Marshal(_modifiedSchema))
											}
											if _ctVal.schema.$ref == _|_ && _ctKey == "multipart/form-data" {
												example_schema: {
													type:       _ctVal.schema.type
													properties: _ctVal.schema.properties
												}
											}
											if _ctVal.example != _|_ {
												example: _ctVal.example
											}
											if _ctVal.examples != _|_ {
												examples: _ctVal.examples
											}
										}]
									}
								}
								if _op.responses != _|_ {
									response_definition: {
										let _responses = *_op.responses | {}
										for _statusCode, _response in _responses {
											"\(_statusCode)": {
												description: _response.description
												if _response.content != _|_ {
													content_type: [for _ct, _ in _response.content {_ct}][0]
													if _response.content[content_type].schema.$ref != _|_ {
														let _parts = strings.Split(_response.content[content_type].schema.$ref, "/")
														ref: strings.ToLower(_parts[len(_parts)-1])
													}
													if _response.content[content_type].schema.$ref == _|_ && _response.content[content_type].schema.allOf != _|_ {
														let _modifiedSchema = {
															allOf: [for _item in _response.content[content_type].schema.allOf {
																if _item.$ref != _|_ {
																	let _parts = strings.Split(_item.$ref, "/")
																	$ref: "#/components/schemas/\(strings.ToLower(_parts[len(_parts)-1]))"
																}
																if _item.$ref == _|_ {
																	_item
																}
															}]
															if _response.content[content_type].schema.description != _|_ {
																description: _response.content[content_type].schema.description
															}
															for _k, _v in _response.content[content_type].schema if _k != "allOf" && _k != "description" {
																"\(_k)": _v
															}
														}
														example_schema: base64.Encode(null, json.Marshal(_modifiedSchema))
													}
													if _response.content[content_type].example != _|_ {
														example: _response.content[content_type].example
													}
												}
											}
										}
									}
								}
							}
						}

						// Hidden fields: used by the operator for filtering and
						// endpoint naming. Not included in the serialized output.
						if _op.operationId != _|_ {
							_operationId: _op.operationId
						}
						if _op.tags != _|_ {
							_tags: _op.tags
						}
					}
				}
			}
		}
	}
}
