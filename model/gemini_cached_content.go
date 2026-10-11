package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"gorm.io/gorm"
)

// GeminiCachedContent binds a gateway-issued explicit context cache id to the
// upstream cache resource and the exact channel credential holding it. A cache
// belongs to the token that created it; requests referencing it are routed to
// that channel and key only.
type GeminiCachedContent struct {
	Id       int    `json:"id"`
	PublicId string `json:"public_id" gorm:"type:varchar(64);uniqueIndex"`
	UserId   int    `json:"user_id" gorm:"index"`
	TokenId  int    `json:"token_id" gorm:"index"`
	// UsingGroup is the group the creation was served and priced in.
	UsingGroup string `json:"using_group" gorm:"type:varchar(64)"`
	ChannelId  int    `json:"channel_id" gorm:"index"`
	KeyIndex   int    `json:"key_index"`
	// KeyFingerprint identifies the credential at KeyIndex so a rotated or
	// reordered key is never used for a cache it does not hold.
	KeyFingerprint string `json:"key_fingerprint" gorm:"type:varchar(64)"`
	UpstreamName   string `json:"upstream_name" gorm:"type:varchar(512)"`
	// Model is the client-facing model name; requests must name it literally.
	Model string `json:"model" gorm:"type:varchar(255)"`
	// UpstreamModel is the model resource the cache was created with upstream.
	UpstreamModel   string         `json:"upstream_model" gorm:"type:varchar(512)"`
	DisplayName     string         `json:"display_name" gorm:"type:varchar(512)"`
	TotalTokenCount int64          `json:"total_token_count"`
	ExpireTime      int64          `json:"expire_time" gorm:"bigint"`
	CreatedTime     int64          `json:"created_time" gorm:"bigint"`
	UpdatedTime     int64          `json:"updated_time" gorm:"bigint"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`
}

const (
	geminiOfficialAPIHost             = "generativelanguage.googleapis.com"
	geminiCachedContentExpireCASRetry = 5
)

var ErrGeminiCachedContentConcurrentUpdate = errors.New("gemini cached content was updated concurrently")

func CreateGeminiCachedContent(record *GeminiCachedContent) error {
	now := common.GetTimestamp()
	record.CreatedTime = now
	record.UpdatedTime = now
	return DB.Create(record).Error
}

// GetGeminiCachedContentForToken returns the live (not deleted) record with
// publicId owned by tokenId, or gorm.ErrRecordNotFound.
func GetGeminiCachedContentForToken(tokenId int, publicId string) (*GeminiCachedContent, error) {
	var record GeminiCachedContent
	err := DB.Where("token_id = ? AND public_id = ?", tokenId, publicId).First(&record).Error
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// ListGeminiCachedContentsForToken returns the token's records that are still
// unexpired at activeAt, newest first. groups limits the result to records
// created in those groups; beforeId > 0 continues a previous page.
func ListGeminiCachedContentsForToken(tokenId int, groups []string, activeAt int64, beforeId int, limit int) ([]*GeminiCachedContent, error) {
	query := DB.Where("token_id = ? AND expire_time > ?", tokenId, activeAt)
	if len(groups) > 0 {
		query = query.Where("using_group IN ?", groups)
	}
	if beforeId > 0 {
		query = query.Where("id < ?", beforeId)
	}
	var records []*GeminiCachedContent
	err := query.Order("id desc").Limit(limit).Find(&records).Error
	return records, err
}

// UpdateGeminiCachedContentExpireTime stores expireTime and returns the value
// it replaced. Concurrent updates each observe their own predecessor, so
// extension charges add up to the expiry actually stored.
func UpdateGeminiCachedContentExpireTime(id int, expireTime int64) (int64, error) {
	for range geminiCachedContentExpireCASRetry {
		var current GeminiCachedContent
		if err := DB.Select("id", "expire_time").Where("id = ?", id).First(&current).Error; err != nil {
			return 0, err
		}
		if current.ExpireTime == expireTime {
			return current.ExpireTime, nil
		}
		result := DB.Model(&GeminiCachedContent{}).
			Where("id = ? AND expire_time = ?", id, current.ExpireTime).
			Updates(map[string]any{"expire_time": expireTime, "updated_time": common.GetTimestamp()})
		if result.Error != nil {
			return 0, result.Error
		}
		if result.RowsAffected == 1 {
			return current.ExpireTime, nil
		}
	}
	return 0, ErrGeminiCachedContentConcurrentUpdate
}

func DeleteGeminiCachedContent(id int) error {
	return DB.Where("id = ?", id).Delete(&GeminiCachedContent{}).Error
}

// SupportsGeminiExplicitCache reports whether explicit context caches can be
// created through this channel: an AI Studio key on the official base URL, a
// gateway declared by other settings, or a Vertex service account. Vertex
// express-mode API keys cannot call the cachedContents resource.
func (channel *Channel) SupportsGeminiExplicitCache() bool {
	switch channel.Type {
	case constant.ChannelTypeGemini:
		return IsOfficialGeminiBaseURL(channel.GetBaseURL()) || channel.GetOtherSettings().GeminiExplicitCacheUpstream
	case constant.ChannelTypeVertexAi:
		return channel.GetOtherSettings().VertexKeyType != dto.VertexKeyTypeAPIKey
	default:
		return false
	}
}

// IsGeminiExplicitCacheGateway reports whether the channel's explicit caches
// live on another gateway, whose rate limits and outages reflect the upstream
// credential holding the cache rather than this channel.
func (channel *Channel) IsGeminiExplicitCacheGateway() bool {
	return channel.Type == constant.ChannelTypeGemini &&
		!IsOfficialGeminiBaseURL(channel.GetBaseURL()) &&
		channel.GetOtherSettings().GeminiExplicitCacheUpstream
}

// GeminiExplicitCacheMaxTTL returns the gateway's TTL limit for explicit
// caches created through this channel; zero means no channel limit.
func (channel *Channel) GeminiExplicitCacheMaxTTL() time.Duration {
	if !channel.IsGeminiExplicitCacheGateway() {
		return 0
	}
	seconds := channel.GetOtherSettings().GeminiExplicitCacheUpstreamMaxTTLSeconds
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// GetEnabledKeyAt returns the key at index when it is still enabled. Single
// key channels hold their key at index 0.
func (channel *Channel) GetEnabledKeyAt(index int) (string, bool) {
	if !channel.ChannelInfo.IsMultiKey {
		return channel.Key, index == 0 && channel.Key != ""
	}
	keys := channel.GetKeys()
	if index < 0 || index >= len(keys) {
		return "", false
	}
	if status, ok := channel.ChannelInfo.MultiKeyStatusList[index]; ok && status != common.ChannelStatusEnabled {
		return "", false
	}
	return keys[index], true
}

// GeminiCacheCredentialFingerprint identifies the Google credential behind a
// channel key. Vertex keys are identified by project and service account so
// reformatting the JSON does not orphan existing caches.
func GeminiCacheCredentialFingerprint(channelType int, key string) string {
	identity := strings.TrimSpace(key)
	if channelType == constant.ChannelTypeVertexAi {
		var credential struct {
			ProjectID    string `json:"project_id"`
			ClientEmail  string `json:"client_email"`
			PrivateKeyID string `json:"private_key_id"`
		}
		if err := common.UnmarshalJsonStr(identity, &credential); err == nil && credential.ProjectID != "" && credential.ClientEmail != "" {
			identity = "vertex:" + credential.ProjectID + "|" + credential.ClientEmail + "|" + credential.PrivateKeyID
		}
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

// IsOfficialGeminiBaseURL reports whether a Gemini channel base URL is the
// official AI Studio endpoint; an empty base URL means the official one.
func IsOfficialGeminiBaseURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "https") &&
		strings.EqualFold(parsed.Hostname(), geminiOfficialAPIHost) &&
		parsed.Port() == "" &&
		strings.TrimRight(parsed.Path, "/") == "" &&
		parsed.RawQuery == ""
}
