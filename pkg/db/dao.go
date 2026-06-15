package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gopass/pkg/crypto"
	"gopass/pkg/types"

	bolt "go.etcd.io/bbolt"
)

var (
	ErrNotFound        = errors.New("record not found")
	ErrTokenMaxReached = errors.New("token usage limit reached")
	ErrTokenInvalid    = errors.New("token is invalid")
)

// --- User DAO Methods ---

// SaveUser saves or updates a User in the users bucket
func (m *Manager) SaveUser(user *types.User) error {
	if user == nil {
		return errors.New("user cannot be nil")
	}
	return m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketUsers)
		data, err := json.Marshal(user)
		if err != nil {
			return fmt.Errorf("failed to marshal user: %w", err)
		}
		key := []byte(strconv.FormatInt(user.UID, 10))
		return b.Put(key, data)
	})
}

// GetUser retrieves a User by their UID. Returns ErrNotFound if not found.
func (m *Manager) GetUser(uid int64) (*types.User, error) {
	var user types.User
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketUsers)
		key := []byte(strconv.FormatInt(uid, 10))
		val := b.Get(key)
		if val == nil {
			return ErrNotFound
		}
		return json.Unmarshal(val, &user)
	})
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// DeleteUser deletes a User by their UID
func (m *Manager) DeleteUser(uid int64) error {
	return m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketUsers)
		key := []byte(strconv.FormatInt(uid, 10))
		return b.Delete(key)
	})
}

// ListUsers retrieves all registered Users (both master and sub)
func (m *Manager) ListUsers() ([]*types.User, error) {
	var users []*types.User
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketUsers)
		return b.ForEach(func(k, v []byte) error {
			var u types.User
			if err := json.Unmarshal(v, &u); err != nil {
				return err
			}
			users = append(users, &u)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return users, nil
}

// --- Token DAO Methods ---

// SaveToken saves or updates a Token in the tokens bucket
func (m *Manager) SaveToken(token *types.Token) error {
	if token == nil {
		return errors.New("token cannot be nil")
	}
	return m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketTokens)
		data, err := json.Marshal(token)
		if err != nil {
			return fmt.Errorf("failed to marshal token: %w", err)
		}
		encrypted := crypto.Encrypt(data)
		return b.Put([]byte(token.Hash), encrypted)
	})
}

// GetToken retrieves a Token by its hash code. Returns ErrNotFound if not found.
func (m *Manager) GetToken(hash string) (*types.Token, error) {
	var token types.Token
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketTokens)
		val := b.Get([]byte(hash))
		if val == nil {
			return ErrNotFound
		}
		decrypted, err := crypto.Decrypt(val)
		if err != nil {
			decrypted = val
		}
		return json.Unmarshal(decrypted, &token)
	})
	if err != nil {
		return nil, err
	}
	return &token, nil
}

// UseToken validates, increments the usage of a token, and registers a sub user.
// This is executed as an atomic transaction.
func (m *Manager) UseToken(hash string, subUID int64) error {
	return m.db.Update(func(tx *bolt.Tx) error {
		// 1. Get token
		tb := tx.Bucket(BucketTokens)
		val := tb.Get([]byte(hash))
		if val == nil {
			return ErrTokenInvalid
		}

		var token types.Token
		decrypted, err := crypto.Decrypt(val)
		if err != nil {
			decrypted = val
		}
		if err := json.Unmarshal(decrypted, &token); err != nil {
			return fmt.Errorf("failed to unmarshal token: %w", err)
		}

		// 2. Validate usage count
		if token.UsedCount >= token.MaxUses {
			return ErrTokenMaxReached
		}

		// 3. Update token usage
		token.UsedCount++
		newData, err := json.Marshal(token)
		if err != nil {
			return fmt.Errorf("failed to marshal updated token: %w", err)
		}
		encryptedToken := crypto.Encrypt(newData)
		if err := tb.Put([]byte(hash), encryptedToken); err != nil {
			return fmt.Errorf("failed to update token: %w", err)
		}

		// 4. Save/register sub user
		ub := tx.Bucket(BucketUsers)
		userKey := []byte(strconv.FormatInt(subUID, 10))

		// Check if user already exists
		existingVal := ub.Get(userKey)
		if existingVal == nil {
			newUser := types.User{
				UID:      subUID,
				Role:     "sub",
				JoinedAt: time.Now(),
			}
			newUserData, err := json.Marshal(&newUser)
			if err != nil {
				return fmt.Errorf("failed to marshal new user: %w", err)
			}
			if err := ub.Put(userKey, newUserData); err != nil {
				return fmt.Errorf("failed to register new user: %w", err)
			}
		}

		return nil
	})
}

// --- ServerNode DAO Methods ---

// SaveNode saves or updates a ServerNode in the nodes bucket
func (m *Manager) SaveNode(node *types.ServerNode) error {
	if node == nil {
		return errors.New("node cannot be nil")
	}
	return m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketNodes)
		data, err := json.Marshal(node)
		if err != nil {
			return fmt.Errorf("failed to marshal node: %w", err)
		}
		return b.Put([]byte(node.Alias), data)
	})
}

// GetNode retrieves a ServerNode by its alias. Returns ErrNotFound if not found.
func (m *Manager) GetNode(alias string) (*types.ServerNode, error) {
	var node types.ServerNode
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketNodes)
		val := b.Get([]byte(alias))
		if val == nil {
			return ErrNotFound
		}
		return json.Unmarshal(val, &node)
	})
	if err != nil {
		return nil, err
	}
	return &node, nil
}

// ListNodes retrieves all registered ServerNodes
func (m *Manager) ListNodes() ([]*types.ServerNode, error) {
	var nodes []*types.ServerNode
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketNodes)
		return b.ForEach(func(k, v []byte) error {
			var n types.ServerNode
			if err := json.Unmarshal(v, &n); err != nil {
				return err
			}
			nodes = append(nodes, &n)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

// --- Config DAO Methods ---

// SetAdminUID sets the Telegram UID of the master administrator in the config bucket
func (m *Manager) SetAdminUID(uid int64) error {
	return m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketConfig)
		val := strconv.FormatInt(uid, 10)
		return b.Put([]byte("admin_uid"), []byte(val))
	})
}

// GetAdminUID retrieves the Telegram UID of the master administrator. Returns ErrNotFound if not set.
func (m *Manager) GetAdminUID() (int64, error) {
	var uid int64
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketConfig)
		val := b.Get([]byte("admin_uid"))
		if val == nil {
			return ErrNotFound
		}
		parsed, err := strconv.ParseInt(string(val), 10, 64)
		if err != nil {
			return fmt.Errorf("failed to parse admin_uid: %w", err)
		}
		uid = parsed
		return nil
	})
	if err != nil {
		return 0, err
	}
	return uid, nil
}

// SetCommunicationToken saves the communication token in the config bucket
func (m *Manager) SetCommunicationToken(token string) error {
	return m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketConfig)
		encrypted := crypto.Encrypt([]byte(token))
		return b.Put([]byte("communication_token"), encrypted)
	})
}

// GetCommunicationToken retrieves the communication token. Returns ErrNotFound if not set.
func (m *Manager) GetCommunicationToken() (string, error) {
	var token string
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketConfig)
		val := b.Get([]byte("communication_token"))
		if val == nil {
			return ErrNotFound
		}
		decrypted, err := crypto.Decrypt(val)
		if err != nil {
			// Fallback to plaintext if decrypt fails
			token = string(val)
		} else {
			token = string(decrypted)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// ListTokens retrieves all active tokens in the database
func (m *Manager) ListTokens() ([]*types.Token, error) {
	var tokens []*types.Token
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketTokens)
		return b.ForEach(func(k, v []byte) error {
			var t types.Token
			decrypted, err := crypto.Decrypt(v)
			if err != nil {
				decrypted = v
			}
			if err := json.Unmarshal(decrypted, &t); err != nil {
				return err
			}
			tokens = append(tokens, &t)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return tokens, nil
}

// SaveCFConfig saves or updates a CFConfig in the cf_configs bucket
func (m *Manager) SaveCFConfig(config *types.CFConfig) error {
	if config == nil {
		return errors.New("cf config cannot be nil")
	}
	return m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketCFConfigs)
		data, err := json.Marshal(config)
		if err != nil {
			return fmt.Errorf("failed to marshal cf config: %w", err)
		}
		encrypted := crypto.Encrypt(data)
		return b.Put([]byte(config.ID), encrypted)
	})
}

// GetCFConfig retrieves a CFConfig by its ID.
func (m *Manager) GetCFConfig(id string) (*types.CFConfig, error) {
	var config types.CFConfig
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketCFConfigs)
		val := b.Get([]byte(id))
		if val == nil {
			return ErrNotFound
		}
		decrypted, err := crypto.Decrypt(val)
		if err != nil {
			// Fallback to plaintext
			decrypted = val
		}
		return json.Unmarshal(decrypted, &config)
	})
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// ListCFConfigs retrieves all registered CFConfigs
func (m *Manager) ListCFConfigs() ([]*types.CFConfig, error) {
	var configs []*types.CFConfig
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketCFConfigs)
		return b.ForEach(func(k, v []byte) error {
			var c types.CFConfig
			decrypted, err := crypto.Decrypt(v)
			if err != nil {
				decrypted = v // Fallback to plaintext
			}
			if err := json.Unmarshal(decrypted, &c); err != nil {
				return err
			}
			configs = append(configs, &c)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return configs, nil
}

// DeleteCFConfig removes a CFConfig by its ID
func (m *Manager) DeleteCFConfig(id string) error {
	return m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketCFConfigs)
		return b.Delete([]byte(id))
	})
}

// MarkNodesOffline updates the connected status to false for a list of node aliases in a single transaction
func (m *Manager) MarkNodesOffline(aliases []string) error {
	if len(aliases) == 0 {
		return nil
	}
	return m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(BucketNodes)
		for _, alias := range aliases {
			val := b.Get([]byte(alias))
			if val == nil {
				continue
			}
			var node types.ServerNode
			if err := json.Unmarshal(val, &node); err != nil {
				continue
			}
			node.Connected = false
			data, err := json.Marshal(&node)
			if err != nil {
				continue
			}
			_ = b.Put([]byte(alias), data)
		}
		return nil
	})
}


