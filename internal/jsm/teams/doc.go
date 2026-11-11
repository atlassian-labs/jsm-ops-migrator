// Package teams is the Teams REST API v1 client: the general part of every
// call under a site's /gateway/api/public/teams/v1/org/<orgId>. It adds the
// site's cloudId as siteId where the API takes it, because the API rejects a
// request without it. The packages that call the Teams API define the paths
// they call and the types they send and decode.
package teams
