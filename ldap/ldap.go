package ldap

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	l "github.com/go-ldap/ldap"
	"github.com/pivotalservices/cf-mgmt/config"
	"github.com/xchapter7x/lo"
)

var (
	attributes = []string{"*"}
)

var (
	userRegexp          = regexp.MustCompile(",[A-Z]+=")
	escapeFilterRegex   = regexp.MustCompile(`([\\\(\)\*\0-\37\177-\377])`)
	unescapeFilterRegex = regexp.MustCompile(`\\([\da-fA-F]{2}|[()\\*])`) // only match \[)*\] or \xx x=a-fA-F
)

const (
	groupFilter                 = "(cn=%s)"
	groupFilterWithObjectClass  = "(&(objectclass=%s)(%s))"
	userFilter                  = "(%s=%s)"
	userFilterWithObjectClass   = "(&(objectclass=%s)(%s=%s))"
	userDNFilter                = "(%s)"
	userDNFilterWithObjectClass = "(&(objectclass=%s)(%s))"
)

func NewManager(ldapConfig *config.LdapConfig) (*Manager, error) {
	conn, err := CreateConnection(ldapConfig)
	if err != nil {
		return nil, err
	}
	return &Manager{
		Config:     ldapConfig,
		Connection: conn,
	}, nil
}

func (m *Manager) GetUserDNs(groupName string) ([]string, error) {
	filter := fmt.Sprintf(groupFilter, l.EscapeFilter(groupName))
	var groupEntry *l.Entry
	lo.G.Debug("Searching for group:", filter)
	lo.G.Debug("Using group search base:", m.Config.GroupSearchBase)

	search := l.NewSearchRequest(
		m.Config.GroupSearchBase,
		l.ScopeWholeSubtree, l.NeverDerefAliases, 0, 0, false,
		filter,
		attributes,
		nil)
	sr, err := m.Connection.Search(search)
	if err != nil {
		lo.G.Error(err)
		return nil, err
	}

	if len(sr.Entries) == 0 {
		lo.G.Errorf("group not found: %s", groupName)
		return []string{}, nil
	}
	if len(sr.Entries) > 1 {
		lo.G.Errorf("multiple groups found for: %s", groupName)
		return []string{}, nil
	}

	groupEntry = sr.Entries[0]
	userDNList := groupEntry.GetAttributeValues(m.Config.GroupAttribute)
	if len(userDNList) == 0 {
		lo.G.Warningf("No users found under group: %s", groupName)
	}

	userMap := make(map[string]string)
	for _, userDN := range userDNList {
		isGroup, nestedGroupName, err := m.IsGroup(userDN)
		if err != nil {
			return nil, err
		}
		if isGroup {
			if err != nil {
				return nil, err
			}
			nestedUsers, err := m.GetUserDNs(nestedGroupName)
			if err != nil {
				return nil, err
			}
			for _, nestedUser := range nestedUsers {
				userMap[nestedUser] = nestedUser
			}
		} else {
			userMap[userDN] = userDN
		}
	}
	var userList []string
	for _, userDN := range userMap {
		userList = append(userList, userDN)
	}
	return userList, nil
}

func (m *Manager) GroupFilter(userDN string) (string, error) {
	indexes := userRegexp.FindStringIndex(strings.ToUpper(userDN))
	if len(indexes) == 0 {
		return "", fmt.Errorf("cannot find CN for DN: %s", userDN)
	}

	cnTemp := UnescapeFilterValue(userDN[:indexes[0]])
	lo.G.Debug("CN unescaped:", cnTemp)

	escapedCN := l.EscapeFilter(strings.Replace(cnTemp, "\\", "", -1))
	lo.G.Debug("CN escaped:", escapedCN)
	groupObjectFilter := "groupOfNames"
	if m.Config.GroupObjectClass != "" {
		groupObjectFilter = m.Config.GroupObjectClass
	}
	return fmt.Sprintf(groupFilterWithObjectClass, groupObjectFilter, escapedCN), nil
}
func (m *Manager) IsGroup(DN string) (bool, string, error) {
	if strings.Contains(DN, m.Config.GroupSearchBase) {
		filter, err := m.GroupFilter(DN)
		if err != nil {
			return false, "", err
		}
		search := l.NewSearchRequest(
			m.Config.GroupSearchBase,
			l.ScopeWholeSubtree, l.NeverDerefAliases, 0, 0, false,
			filter,
			attributes,
			nil)
		sr, err := m.Connection.Search(search)
		if err != nil {
			return false, "", err
		}
		lo.G.Debugf("Found %d entries for group filter %s", len(sr.Entries), filter)
		if len(sr.Entries) == 1 {
			return true, sr.Entries[0].GetAttributeValue("cn"), nil
		}
		return false, "", nil
	} else {
		return false, "", nil
	}
}

func (m *Manager) GetUserByDN(userDN string) (*User, error) {
	lo.G.Debug("User DN:", userDN)
	indexes := userRegexp.FindStringIndex(strings.ToUpper(userDN))
	if len(indexes) == 0 {
		return nil, fmt.Errorf("cannot find CN for user DN: %s", userDN)
	}
	index := indexes[0]
	userCNTemp := UnescapeFilterValue(userDN[:index])
	lo.G.Debug("CN unescaped:", userCNTemp)

	userCN := l.EscapeFilter(strings.Replace(userCNTemp, "\\", "", 1))
	lo.G.Debug("CN escaped:", userCN)
	filter := m.getUserFilterWithCN(userCN)
	return m.searchUser(filter, userDN[index+1:], "")
}

func (m *Manager) GetUserByID(userID string) (*User, error) {
	filter := m.getUserFilter(userID)
	lo.G.Debug("Searching for user:", filter)
	lo.G.Debug("Using user search base:", m.Config.UserSearchBase)
	return m.searchUser(filter, m.Config.UserSearchBase, userID)
}

func (m *Manager) searchUser(filter, searchBase, userID string) (*User, error) {
	lo.G.Debugf("Searching with filter %s", filter)
	search := l.NewSearchRequest(
		searchBase,
		l.ScopeWholeSubtree, l.NeverDerefAliases, 0, 0, false,
		filter,
		attributes,
		nil)

	sr, err := m.Connection.Search(search)
	if err != nil {
		lo.G.Error(err)
		return nil, err
	}

	if (len(sr.Entries)) == 1 {
		entry := sr.Entries[0]
		user := &User{
			UserDN: entry.DN,
			Email:  entry.GetAttributeValue(m.Config.UserMailAttribute),
		}
		if userID != "" {
			user.UserID = userID
		} else {
			user.UserID = entry.GetAttributeValue(m.Config.UserNameAttribute)
		}
		lo.G.Debugf("Search filter %s returned userDN [%s], email [%s], userID [%s]", filter, user.UserDN, user.Email, user.UserID)
		return user, nil
	}
	lo.G.Errorf("Found %d number of entries for filter %s", len(sr.Entries), filter)
	return nil, nil
}

func UnescapeFilterValue(filter string) string {
	repl := unescapeFilterRegex.ReplaceAllFunc(
		[]byte(filter),
		func(match []byte) []byte {
			// \( \) \\ \*
			if len(match) == 2 {
				return []byte{match[1]}
			}
			// had issues with Decode, TODO fix to use Decode?.
			res, _ := hex.DecodeString(string(match[1:]))
			return res
		},
	)
	return string(repl)
}

func (m *Manager) getUserFilter(userID string) string {
	if m.Config.UserObjectClass == "" {
		return fmt.Sprintf(userFilter, m.Config.UserNameAttribute, userID)
	}
	return fmt.Sprintf(userFilterWithObjectClass, m.Config.UserObjectClass, m.Config.UserNameAttribute, userID)
}

func (m *Manager) getUserFilterWithCN(cn string) string {
	if m.Config.UserObjectClass == "" {
		return fmt.Sprintf(userDNFilter, cn)
	}
	return fmt.Sprintf(userDNFilterWithObjectClass, m.Config.UserObjectClass, cn)
}

func (m *Manager) Close() {
	if m.Connection != nil {
		m.Connection.Close()
	}
}
