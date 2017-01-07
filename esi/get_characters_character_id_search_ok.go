/* 
 * EVE Swagger Interface
 *
 * An OpenAPI for EVE Online
 *
 * OpenAPI spec version: 0.3.6.dev9
 * 
 * Generated by: https://github.com/swagger-api/swagger-codegen.git
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package esi

// 200 ok object
type GetCharactersCharacterIdSearchOk struct {

	// agent array
	Agent []int32 `json:"agent,omitempty"`

	// alliance array
	Alliance []int32 `json:"alliance,omitempty"`

	// character array
	Character []int32 `json:"character,omitempty"`

	// constellation array
	Constellation []int32 `json:"constellation,omitempty"`

	// corporation array
	Corporation []int32 `json:"corporation,omitempty"`

	// faction array
	Faction []int32 `json:"faction,omitempty"`

	// inventorytype array
	Inventorytype []int32 `json:"inventorytype,omitempty"`

	// region array
	Region []int32 `json:"region,omitempty"`

	// solarsystem array
	Solarsystem []int32 `json:"solarsystem,omitempty"`

	// station array
	Station []int32 `json:"station,omitempty"`

	// structure array
	Structure []int64 `json:"structure,omitempty"`

	// wormhole array
	Wormhole []int32 `json:"wormhole,omitempty"`
}
