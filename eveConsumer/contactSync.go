package eveConsumer

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/antihax/evedata/models"
	"github.com/antihax/goesi"
	"github.com/antihax/goesi/v1"
	"github.com/antihax/goesi/v3"
	"github.com/antihax/goesi/v4"
	"github.com/garyburd/redigo/redis"

	"golang.org/x/oauth2"
)

func init() {
	addConsumer("contactSync", contactSyncConsumer, "EVEDATA_contactSyncQueue")
	addTrigger("contactSync", contactSyncTrigger)
}

// Perform contact sync for wardecs
func contactSyncTrigger(c *EVEConsumer) (bool, error) {

	// Do quick maintenence to prevent errors.
	err := models.MaintContactSync()
	if err != nil {
		return false, err
	}

	// Gather characters for update. Group for optimized updating.
	rows, err := c.ctx.Db.Query(
		`SELECT S.characterID, source, group_concat(destination)
			FROM evedata.contactSyncs S  
            INNER JOIN evedata.crestTokens T ON T.tokenCharacterID = destination
            WHERE lastStatus NOT LIKE "%400 Bad Request%"
		    GROUP BY source
            HAVING max(nextSync) < UTC_TIMESTAMP();`)

	if err != nil && err != sql.ErrNoRows {
		return false, err
	} else if err == sql.ErrNoRows { // Shut up warnings
		return false, nil
	}
	defer rows.Close()

	r := c.ctx.Cache.Get()
	defer r.Close()

	// Loop updatable characters
	for rows.Next() {
		var (
			characterID int64
			source      int64  // Source char
			dest        string // List of destination chars
		)

		err = rows.Scan(&characterID, &source, &dest)
		if err != nil {
			log.Printf("Contact Sync: Failed scan: %v", err)
			continue
		}

		_, err = r.Do("SADD", "EVEDATA_contactSyncQueue", fmt.Sprintf("%d:%d:%s", characterID, source, dest))
		if err != nil {
			log.Printf("Contact Sync: Failed scan: %v", err)
			continue
		}
	}
	return true, err
}

func contactSyncConsumer(c *EVEConsumer, redisPtr *redis.Conn) (bool, error) {
	r := *redisPtr
	ret, err := r.Do("SPOP", "EVEDATA_contactSyncQueue")
	if err != nil {
		return false, err
	} else if ret == nil {
		return false, nil
	}
	v, err := redis.String(ret, err)
	if err != nil {
		return false, err
	}

	// Split off characters into an array
	dest := strings.Split(v, ":")
	destinations := strings.Split(dest[2], ",")
	characterID, err := strconv.ParseInt(dest[0], 10, 64)
	if err != nil {
		return false, err
	}
	source, err := strconv.ParseInt(dest[1], 10, 64)
	if err != nil {
		return false, err
	}

	var char goesiv4.GetCharactersCharacterIdOk
	for {
		var (
			r   *http.Response
			err error
		)
		// get the source character information
		char, r, err = c.ctx.ESI.V4.CharacterApi.GetCharactersCharacterId((int32)(source), nil)
		if err != nil {
			// Retry on their failure
			if r != nil && r.StatusCode >= 500 {
				continue
			}
			return false, err
		}
		break
	}

	var corp goesiv3.GetCorporationsCorporationIdOk
	for {
		var (
			r   *http.Response
			err error
		)
		corp, r, err = c.ctx.ESI.V3.CorporationApi.GetCorporationsCorporationId(char.CorporationId, nil)
		if err != nil {
			// Retry on their failure
			if r != nil && r.StatusCode >= 500 {
				continue
			}
			return false, err
		}
		break
	}

	// Find the Entity ID to search for wars.
	var searchID int32
	if corp.AllianceId > 0 {
		searchID = corp.AllianceId
	} else {
		searchID = char.CorporationId
	}

	// Map of tokens
	type characterToken struct {
		token *oauth2.TokenSource
		cid   int64
	}
	tokens := make(map[int64]characterToken)

	// Get the tokens for our destinations
	for _, cidS := range destinations {
		cid, _ := strconv.ParseInt(cidS, 10, 64)
		a, err := c.getToken(characterID, cid)
		if err != nil {
			return false, err
		}
		// Save the token.
		tokens[cid] = characterToken{token: &a, cid: cid}
	}

	// Active Wars
	activeWars, err := models.GetActiveWarsByID((int64)(searchID))
	if err != nil {
		return false, err
	}

	// Pending Wars
	pendingWars, err := models.GetPendingWarsByID((int64)(searchID))
	if err != nil {
		return false, err
	}

	// Faction Wars
	var factionWars []models.FactionWarEntities
	if corp.Faction != "" {
		factionWars, err = models.GetFactionWarEntitiesForID(models.FactionsByName[corp.Faction])
		if err != nil {
			return false, err
		}
	}

	// Loop through all the destinations
	for _, token := range tokens {
		// authentication token context for destination char
		auth := context.WithValue(context.TODO(), goesi.ContextOAuth2, *token.token)
		var (
			contacts []goesiv1.GetCharactersCharacterIdContacts200Ok
			r        *http.Response
			err      error
		)
		// Default to OK
		tokenSuccess(source, token.cid, 200, "OK")

		// Get current contacts
		for i := 1; ; i++ {
			var con []goesiv1.GetCharactersCharacterIdContacts200Ok
			con, r, err = c.ctx.ESI.V1.ContactsApi.GetCharactersCharacterIdContacts(auth, (int32)(token.cid), map[string]interface{}{"page": (int32)(i)})
			if err != nil || r.StatusCode != 200 {
				tokenError(source, token.cid, r, err)
				return false, err
			}
			if len(con) == 0 {
				break
			}
			contacts = append(contacts, con...)
		}

		// Update cache time.
		if r != nil {
			contactSync := &models.ContactSync{Source: source, Destination: token.cid}
			err := contactSync.Updated(time.Now().UTC().Add(time.Second * 900))
			if err != nil {
				return false, err
			}
		}

		var erase []int32
		var active []int32
		var pending []int32
		var pendingMove []int32
		var activeMove []int32
		var untouchableContacts int

		// Figure out how many contacts they have outside of ours
		for _, contact := range contacts {
			if contact.Standing > -0.4 {
				untouchableContacts++
			}
		}

		// Faction wars can get over the 1024 contact limit so we need to trim
		// real wars will take precedence.
		trim := len(activeWars) + len(pendingWars)

		activeCheck := make(map[int32]bool)
		pendingCheck := make(map[int32]bool)

		// Build a map of active wars
		for _, war := range activeWars {
			activeCheck[(int32)(war.ID)] = true
		}

		// Add faction wars to the active list
		maxFactionWarLength := min(980-trim-untouchableContacts, len(factionWars))
		for _, war := range factionWars[:maxFactionWarLength] {
			activeCheck[(int32)(war.ID)] = true
		}

		// Build a map of pending wars
		for _, war := range pendingWars {
			pendingCheck[(int32)(war.ID)] = true
		}

		// Loop through all current contacts and figure out needed moves
		for _, contact := range contacts {
			// skip anything > -0.4
			if contact.Standing > -0.4 {
				continue
			}

			pend := pendingCheck[contact.ContactId]
			act := activeCheck[contact.ContactId]

			// Is this existing contact in the active list
			if !act {
				// Is this existing contact in the pending list
				if !pend { // Not in either list. delete it.
					erase = append(erase, (int32)(contact.ContactId))
				} else if pend && contact.Standing > -5.0 { // in pending list but wrong standing
					// Take it out of the active list and put into pending move.
					delete(pendingCheck, contact.ContactId)
					pendingMove = append(pendingMove, (int32)(contact.ContactId))
				} else if pend && contact.Standing == -5.0 { // Contact correct, do nothing.
					delete(pendingCheck, contact.ContactId)
				}
			} else if act && contact.Standing != -10.0 { // in active list, but wrong standing
				delete(activeCheck, contact.ContactId)
				activeMove = append(activeMove, (int32)(contact.ContactId))
			} else if act && contact.Standing == -10.0 { // Contact correct, do nothing.
				delete(activeCheck, contact.ContactId)
			}
		}
		// Build a list of active wars to add
		for con := range activeCheck {
			active = append(active, con)
		}

		// Build a list of pending wars to add
		for con := range pendingCheck {
			pending = append(pending, con)
		}

		// Erase contacts which have no wars.
		if len(erase) > 0 {
			for start := 0; start < len(erase); start = start + 20 {
				end := min(start+20, len(erase))
				failure := 0
				for {
					r, err = c.ctx.ESI.V1.ContactsApi.DeleteCharactersCharacterIdContacts(auth, (int32)(token.cid), erase[start:end], nil)
					if err != nil {
						var resb []byte
						if r != nil {
							resb, _ = httputil.DumpResponse(r, true)
						}
						log.Printf("ContactSync: Error Erasing %d %s %s\n", token.cid, err, resb)
						// Retry on their failure
						if failure > 5 {
							break
						} else if r != nil && r.StatusCode >= 500 {
							continue
							failure++
						}
						return false, err
					}
					break
				}
			}
		}
		// Add contacts for active wars
		if len(active) > 0 {
			for start := 0; start < len(active); start = start + 100 {
				end := min(start+100, len(active))
				failure := 0
				for {
					_, r, err = c.ctx.ESI.V1.ContactsApi.PostCharactersCharacterIdContacts(auth, (int32)(token.cid), active[start:end], -10, nil)
					if err != nil {
						var resb []byte
						if r != nil {
							resb, _ = httputil.DumpResponse(r, true)
						}
						log.Printf("ContactSync: Error Adding Active %d %s %s\n", token.cid, err, resb)
						// Retry on their failure
						if failure > 5 {
							break
						} else if r != nil && r.StatusCode >= 500 {
							continue
							failure++
						}
						return false, err
					}
					break
				}
			}
		}
		// Add contacts for pending wars
		if len(pending) > 0 {
			for start := 0; start < len(pending); start = start + 100 {
				end := min(start+100, len(pending))
				failure := 0
				for {
					_, r, err = c.ctx.ESI.V1.ContactsApi.PostCharactersCharacterIdContacts(auth, (int32)(token.cid), pending[start:end], -5, nil)
					if err != nil {
						var resb []byte
						if r != nil {
							resb, _ = httputil.DumpResponse(r, true)
						}
						log.Printf("ContactSync: Error Adding Pending %s %s\n", err, resb)
						// Retry on their failure
						if failure > 5 {
							break
						} else if r != nil && r.StatusCode >= 500 {
							continue
							failure++
						}
						return false, err
					}
					break
				}
			}
		}
		// Move contacts to active wars
		if len(activeMove) > 0 {
			for start := 0; start < len(activeMove); start = start + 20 {
				end := min(start+20, len(activeMove))
				failure := 0
				for {
					r, err = c.ctx.ESI.V1.ContactsApi.PutCharactersCharacterIdContacts(auth, (int32)(token.cid), activeMove[start:end], -10, nil)
					if err != nil {
						var resb []byte
						if r != nil {
							resb, _ = httputil.DumpResponse(r, true)
						}
						log.Printf("ContactSync: Error Moving Active %s %s\n", err, resb)
						// Retry on their failure
						if failure > 5 {
							break
						} else if r != nil && r.StatusCode >= 500 {
							continue
							failure++
						}
						return false, err
					}
					break
				}
			}
		}
		// Move contacts to pending wars
		if len(pendingMove) > 0 {
			for start := 0; start < len(pendingMove); start = start + 20 {
				end := min(start+20, len(pendingMove))
				failure := 0
				for {
					r, err = c.ctx.ESI.V1.ContactsApi.PutCharactersCharacterIdContacts(auth, (int32)(token.cid), pendingMove[start:end], -5, nil)
					if err != nil {
						// Retry on their failure
						if failure > 5 {
							break
						} else if r != nil && r.StatusCode >= 500 {
							continue
							failure++
						}
						return false, err
					}
					break
				}
			}
		}
	}
	return true, err
}
