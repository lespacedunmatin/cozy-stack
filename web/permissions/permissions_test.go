package permissions

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/instance"
	"github.com/cozy/cozy-stack/pkg/oauth"
	"github.com/cozy/cozy-stack/pkg/permissions"
	"github.com/cozy/cozy-stack/web/jsonapi"
	"github.com/labstack/echo"
	"github.com/stretchr/testify/assert"
	jwt "gopkg.in/dgrijalva/jwt-go.v3"
)

var ts *httptest.Server
var token string
var testInstance *instance.Instance
var clientID string

func TestMain(m *testing.M) {
	config.UseTestFile()

	testInstance = &instance.Instance{
		OAuthSecret: []byte("topsecret"),
		Domain:      "example.com",
	}

	err := couchdb.ResetDB(testInstance, "io.cozy.events")
	if err != nil {
		fmt.Println("Cant reset db", err)
		os.Exit(1)
	}

	err = couchdb.ResetDB(testInstance, consts.Permissions)
	if err != nil {
		fmt.Println("Cant reset db", err)
		os.Exit(1)
	}
	err = couchdb.DefineIndexes(testInstance, consts.IndexesByDoctype(consts.Permissions))
	if err != nil {
		fmt.Println("Cant define index", err)
		os.Exit(1)
	}
	err = couchdb.DefineViews(testInstance, consts.ViewsByDoctype(consts.Permissions))
	if err != nil {
		fmt.Println("cant define views", err)
		os.Exit(1)
	}

	client := oauth.Client{
		RedirectURIs: []string{"http://localhost/oauth/callback"},
		ClientName:   "test-permissions",
		SoftwareID:   "github.com/cozy/cozy-stack/web/permissions",
	}
	client.Create(testInstance)
	clientID = client.ClientID

	token, _ = crypto.NewJWT(testInstance.OAuthSecret, permissions.Claims{
		StandardClaims: jwt.StandardClaims{
			Audience: permissions.AccessTokenAudience,
			Issuer:   testInstance.Domain,
			IssuedAt: crypto.Timestamp(),
			Subject:  clientID,
		},
		Scope: "io.cozy.contacts io.cozy.files:GET",
	})

	handler := echo.New()

	group := handler.Group("/permissions")
	group.Use(injectInstance(testInstance))
	Routes(group)

	ts = httptest.NewServer(handler)
	res := m.Run()
	ts.Close()
	os.Exit(res)
}

func TestGetPermissions(t *testing.T) {
	req, _ := http.NewRequest("GET", ts.URL+"/permissions/self", nil)
	req.Header.Add("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if !assert.NoError(t, err) {
		return
	}
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if !assert.NoError(t, err) {
		return
	}
	var out map[string]interface{}
	err = json.Unmarshal(body, &out)

	assert.NoError(t, err)
	assert.Equal(t, "200 OK", res.Status, "should get a 200")
	for key, r := range out {
		rule := r.(map[string]interface{})
		if key == "rule0" {
			assert.Equal(t, "io.cozy.contacts", rule["type"])
		} else {
			assert.Equal(t, "io.cozy.files", rule["type"])
			assert.Equal(t, []interface{}{"GET"}, rule["verbs"])
		}
	}
}

func TestGetPermissionsForRevokedClient(t *testing.T) {
	tok, err := crypto.NewJWT(testInstance.OAuthSecret, permissions.Claims{
		StandardClaims: jwt.StandardClaims{
			Audience: permissions.AccessTokenAudience,
			Issuer:   testInstance.Domain,
			IssuedAt: crypto.Timestamp(),
			Subject:  "revoked-client",
		},
		Scope: "io.cozy.contacts io.cozy.files:GET",
	})
	assert.NoError(t, err)
	req, _ := http.NewRequest("GET", ts.URL+"/permissions/self", nil)
	req.Header.Add("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 400, res.StatusCode)
	body, err := ioutil.ReadAll(res.Body)
	assert.NoError(t, err)
	assert.Equal(t, `{"message":"Invalid JWT token"}`, string(body))
}

func TestGetPermissionsForExpiredToken(t *testing.T) {
	pastTimestamp := crypto.Timestamp() - 30*24*3600 // in seconds
	tok, err := crypto.NewJWT(testInstance.OAuthSecret, permissions.Claims{
		StandardClaims: jwt.StandardClaims{
			Audience: permissions.AccessTokenAudience,
			Issuer:   testInstance.Domain,
			IssuedAt: pastTimestamp,
			Subject:  clientID,
		},
		Scope: "io.cozy.contacts io.cozy.files:GET",
	})
	assert.NoError(t, err)
	req, _ := http.NewRequest("GET", ts.URL+"/permissions/self", nil)
	req.Header.Add("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 400, res.StatusCode)
	body, err := ioutil.ReadAll(res.Body)
	assert.NoError(t, err)
	assert.Equal(t, `{"message":"Expired token"}`, string(body))
}

func TestBadPermissionsBearer(t *testing.T) {
	req, _ := http.NewRequest("GET", ts.URL+"/permissions/self", nil)
	req.Header.Add("Authorization", "Bearer garbage")
	res, err := http.DefaultClient.Do(req)
	if !assert.NoError(t, err) {
		return
	}
	defer res.Body.Close()
	assert.Equal(t, res.StatusCode, http.StatusBadRequest)
}

func TestCreateSubPermission(t *testing.T) {
	_, codes, err := createTestSubPermissions(token, "alice,bob")
	if !assert.NoError(t, err) {
		return
	}

	aCode := codes["alice"].(string)
	bCode := codes["bob"].(string)

	assert.NotEqual(t, aCode, token)
	assert.NotEqual(t, bCode, token)
	assert.NotEqual(t, aCode, bCode)

	req, _ := http.NewRequest("GET", ts.URL+"/permissions/self", nil)
	req.Header.Add("Authorization", "Bearer "+aCode)
	res, err := http.DefaultClient.Do(req)
	if !assert.NoError(t, err) {
		return
	}
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if !assert.NoError(t, err) {
		return
	}
	var out map[string]interface{}
	err = json.Unmarshal(body, &out)

	assert.NoError(t, err)
	assert.Equal(t, "200 OK", res.Status, "should get a 200")
	assert.Len(t, out, 1)
	assert.Equal(t, "io.cozy.files", out["whatever"].(map[string]interface{})["type"])

}

func TestCreateSubSubFail(t *testing.T) {
	_, codes, err := createTestSubPermissions(token, "eve")
	if !assert.NoError(t, err) {
		return
	}
	eveCode := codes["eve"].(string)
	_, _, err = createTestSubPermissions(eveCode, "eve")
	if !assert.Error(t, err) {
		return
	}
}

func TestPatchNoopFail(t *testing.T) {
	id, _, err := createTestSubPermissions(token, "pierre")
	if !assert.NoError(t, err) {
		return
	}

	_, err = doRequest("PATCH", ts.URL+"/permissions/"+id, token, `{
	  "data": {
	    "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6",
	    "type": "io.cozy.permissions",
	    "attributes": { }
	    }
	  }
	}
`)

	assert.Error(t, err)
}

func TestBadPatchAddRuleForbidden(t *testing.T) {
	id, _, err := createTestSubPermissions(token, "jacque")
	if !assert.NoError(t, err) {
		return
	}

	_, err = doRequest("PATCH", ts.URL+"/permissions/"+id, token, `{
	  "data": {
	    "attributes": {
					"permissions": {
						"otherperm": {
							"type":"io.cozy.token-cant-do-this"
						}
					}
				}
	    }
	  }
`)

	assert.Error(t, err)
}

func TestPatchAddRule(t *testing.T) {
	id, _, err := createTestSubPermissions(token, "paul")
	if !assert.NoError(t, err) {
		return
	}

	out, err := doRequest("PATCH", ts.URL+"/permissions/"+id, token, `{
	  "data": {
	    "attributes": {
					"permissions": {
						"otherperm": {
							"type":"io.cozy.contacts"
						}
					}
				}
	    }
	  }
`)

	data := out["data"].(map[string]interface{})
	assert.Equal(t, id, data["id"])
	attrs := data["attributes"].(map[string]interface{})
	perms := attrs["permissions"].(map[string]interface{})

	assert.NoError(t, err)
	assert.Len(t, perms, 2)
	assert.Equal(t, "io.cozy.files", perms["whatever"].(map[string]interface{})["type"])
	assert.Equal(t, "io.cozy.contacts", perms["otherperm"].(map[string]interface{})["type"])
}

func TestPatchChangesCodes(t *testing.T) {
	id, codes, err := createTestSubPermissions(token, "john,jane")
	if !assert.NoError(t, err) {
		return
	}

	assert.NotEmpty(t, codes["john"])
	janeToken := codes["jane"].(string)
	assert.NotEmpty(t, janeToken)

	_, err = doRequest("PATCH", ts.URL+"/permissions/"+id, janeToken, `{
		"data": {
			"attributes": {
					"codes": {
						"john": "set-token"
					}
				}
			}
		}
`)
	assert.Error(t, err)

	out, err := doRequest("PATCH", ts.URL+"/permissions/"+id, token, `{
	  "data": {
	    "attributes": {
					"codes": {
						"john": "set-token"
					}
				}
	    }
	  }
`)

	if !assert.NoError(t, err) {
		return
	}
	data := out["data"].(map[string]interface{})
	assert.Equal(t, id, data["id"])
	attrs := data["attributes"].(map[string]interface{})
	newcodes := attrs["codes"].(map[string]interface{})
	assert.NotEmpty(t, newcodes["john"])
	assert.Nil(t, newcodes["jane"])

}

func TestRevoke(t *testing.T) {
	id, codes, err := createTestSubPermissions(token, "igor")
	if !assert.NoError(t, err) {
		return
	}

	igorToken := codes["igor"].(string)
	assert.NotEmpty(t, igorToken)

	_, err = doRequest("DELETE", ts.URL+"/permissions/"+id, igorToken, "")
	assert.Error(t, err)

	out, err := doRequest("DELETE", ts.URL+"/permissions/"+id, token, "")
	assert.NoError(t, err)
	assert.Nil(t, out)

}

func createTestSubPermissions(tok string, codes string) (string, map[string]interface{}, error) {
	out, err := doRequest("POST", ts.URL+"/permissions?codes="+codes, tok, `{
"data": {
	"type": "io.cozy.permissions",
	"attributes": {
		"permissions": {
			"whatever": {
				"type":   "io.cozy.files",
				"verbs":  ["GET"],
				"values": ["io.cozy.music"]
			}
		}
	}
}
	}`)

	if err != nil {
		return "", nil, err
	}

	data := out["data"].(map[string]interface{})
	id := data["id"].(string)
	attrs := data["attributes"].(map[string]interface{})
	result := attrs["codes"].(map[string]interface{})
	return id, result, nil
}

func doRequest(method, url, tok, body string) (map[string]interface{}, error) {
	reqbody := strings.NewReader(body)
	req, _ := http.NewRequest(method, url, reqbody)
	req.Header.Add("Authorization", "Bearer "+tok)
	req.Header.Add("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	resbody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if len(resbody) == 0 {
		return nil, nil
	}
	var out map[string]interface{}
	err = json.Unmarshal(resbody, &out)
	if err != nil {
		return nil, err
	}

	if errstr := out["message"]; errstr != nil {
		return nil, fmt.Errorf("%s", errstr)
	}

	return out, nil
}

func TestListPermission(t *testing.T) {

	ev1, _ := createTestEvent(testInstance)
	ev2, _ := createTestEvent(testInstance)
	ev3, _ := createTestEvent(testInstance)

	parent, _ := permissions.GetForOauth(&permissions.Claims{
		StandardClaims: jwt.StandardClaims{
			Audience: permissions.AccessTokenAudience,
			Issuer:   testInstance.Domain,
			IssuedAt: crypto.Timestamp(),
			Subject:  clientID,
		},
		Scope: "io.cozy.events",
	})
	p1 := permissions.Set{
		permissions.Rule{
			Type:   "io.cozy.events",
			Verbs:  permissions.Verbs(permissions.DELETE, permissions.PATCH),
			Values: []string{ev1.ID()},
		}}
	p2 := permissions.Set{
		permissions.Rule{
			Type:   "io.cozy.events",
			Verbs:  permissions.Verbs(permissions.GET),
			Values: []string{ev2.ID()},
		}}

	codes := map[string]string{"bob": "secret"}
	permissions.CreateShareSet(testInstance, parent, codes, p1)
	permissions.CreateShareSet(testInstance, parent, codes, p2)

	reqbody := strings.NewReader(`{
"data": [
{ "type": "io.cozy.events", "id": "` + ev1.ID() + `" },
{ "type": "io.cozy.events", "id": "` + ev2.ID() + `" },
{ "type": "io.cozy.events", "id": "non-existing-id" },
{ "type": "io.cozy.events", "id": "another-fake-id" },
{ "type": "io.cozy.events", "id": "` + ev3.ID() + `" }
]	}`)

	req, _ := http.NewRequest("POST", ts.URL+"/permissions/exists", reqbody)
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if !assert.NoError(t, err) {
		return
	}
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if !assert.NoError(t, err) {
		return
	}
	var out jsonapi.Document
	err = json.Unmarshal(body, &out)
	if !assert.NoError(t, err) {
		return
	}
	var results []refAndVerb
	err = json.Unmarshal(*out.Data, &results)
	if !assert.NoError(t, err) {
		return
	}

	assert.Len(t, results, 2)
	for _, result := range results {
		assert.Equal(t, "io.cozy.events", result.DocType)
		if result.ID == ev1.ID() {
			assert.Equal(t, "PATCH,DELETE", result.Verbs.String())
		} else {
			assert.Equal(t, ev2.ID(), result.ID)
			assert.Equal(t, "GET", result.Verbs.String())
		}
	}

}

func injectInstance(i *instance.Instance) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set("instance", i)
			return next(c)
		}
	}
}

func createTestEvent(i *instance.Instance) (*couchdb.JSONDoc, error) {
	e := &couchdb.JSONDoc{
		Type: "io.cozy.events",
		M:    map[string]interface{}{"test": "value"},
	}
	err := couchdb.CreateDoc(i, e)
	return e, err
}
