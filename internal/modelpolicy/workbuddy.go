package modelpolicy

// DefaultWorkBuddyModelID is the gateway-side default for the WorkBuddy
// international channel, used when a request arrives without a model name.
//
// It is a routing default, not a catalog. Which models exist is decided by the
// `cli` agent whitelist read from GET /v3/config during a refresh. A compiled-in
// list is deliberately absent: it would keep a withdrawn model served, and it
// would make an account that never answered the catalog read look populated.
const DefaultWorkBuddyModelID = "default-model"
