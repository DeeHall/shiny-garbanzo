package slackclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"time"
)

type Cursor struct {
	NextCursor string `json:"next_cursor"`
}

type CursorResponseMetadata struct {
	ResponseMetadata Cursor `json:"response_metadata"`
}

type Attachment struct {
	ID   int
	Text string
}

type Message struct {
	User        string
	BotID       string `json:"bot_id"`
	Text        string
	Attachments []Attachment
	Ts          string
	Type        string
}

type HistoryResponse struct {
	CursorResponseMetadata
	Ok       bool
	HasMore  bool `json:"has_more"`
	Messages []Message
}

type Channel struct {
	ID         string
	Name       string
	Is_Channel bool
}

type ChannelInfoResponse struct {
	Ok      bool
	Channel Channel
}

type ConversationsResponse struct {
	CursorResponseMetadata
	Ok       bool
	Channels []Channel
}

type User struct {
	ID   string
	Name string
}

type UsersResponse struct {
	Ok      bool
	Members []User
}

type Cache struct {
	Channels map[string]string
	Users    map[string]string
}

type SlackClient struct {
	cachePath string
	team      string
	client    http.Client
	auth      *SlackAuth
	cache     Cache
	log       *log.Logger
}

func New(team string, log *log.Logger) (*SlackClient, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dataHome = path.Join(home, ".local", ".share")
	}
	cachePath := path.Join(dataHome, "gh-slack")

	auth, err := getSlackAuthFromEnv()
	if err != nil {
		return nil, err
	}

	c := &SlackClient{
		cachePath: cachePath,
		team:      team,
		auth:      auth,
		log:       log,
	}

	err = c.loadCache()
	return c, err
}

func (c *SlackClient) get(path string, params map[string]string) ([]byte, error) {
	u, err := url.Parse(fmt.Sprintf("https://%s.slack.com/api/", c.team))
	if err != nil {
		return nil, err
	}
	u.Path += path
	q := u.Query()
	q.Add("token", c.auth.Token)
	for p := range params {
		q.Add(p, params[p])
	}
	u.RawQuery = q.Encode()

	var body []byte
	for {
		req, err := http.NewRequest("GET", u.String(), nil)
		if err != nil {
			return nil, err
		}
		for key := range c.auth.Cookies {
			req.AddCookie(&http.Cookie{Name: key, Value: c.auth.Cookies[key]})
		}

		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}

		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == 429 {
			s, err := strconv.Atoi(resp.Header["Retry-After"][0])
			if err != nil {
				return nil, err
			}
			d := time.Duration(s)
			c.log.Printf("rate limited, waiting %ds", d)
			time.Sleep(d * time.Second)
		} else if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("status code %d, headers: %q, body: %q", resp.StatusCode, resp.Header, body)
		} else {
			break
		}
	}

	return body, nil
}

func (c *SlackClient) ChannelInfo(id string) (*Channel, error) {
	body, err := c.get("conversations.info",
		map[string]string{"channel": id})
	if err != nil {
		return nil, err
	}

	channelInfoReponse := &ChannelInfoResponse{}
	err = json.Unmarshal(body, channelInfoReponse)
	if err != nil {
		return nil, err
	}

	if !channelInfoReponse.Ok {
		return nil, fmt.Errorf("conversations.info response not OK: %s", body)
	}

	return &channelInfoReponse.Channel, nil
}

func (c *SlackClient) conversations(params map[string]string) ([]Channel, error) {
	channels := make([]Channel, 0, 1000)
	conversations := &ConversationsResponse{}
	for {
		c.log.Printf("Fetching conversations with cursor %q", conversations.ResponseMetadata.NextCursor)
		body, err := c.get("conversations.list",
			map[string]string{
				"cursor":           conversations.ResponseMetadata.NextCursor,
				"exclude_archived": "true"},
		)
		if err != nil {
			return nil, err
		}

		if err = json.Unmarshal(body, conversations); err != nil {
			return nil, err
		}

		if !conversations.Ok {
			return nil, fmt.Errorf("conversations response not OK: %s", body)
		}

		channels = append(channels, conversations.Channels...)
		c.log.Printf("Fetched %d channels (total so far %d)",
			len(conversations.Channels),
			len(channels))

		if conversations.ResponseMetadata.NextCursor == "" {
			break
		}
	}

	return channels, nil
}

func (c *SlackClient) users(params map[string]string) (*UsersResponse, error) {
	body, err := c.get("users.list", nil)
	if err != nil {
		return nil, err
	}

	users := &UsersResponse{}
	err = json.Unmarshal(body, users)
	if err != nil {
		return nil, err
	}

	if !users.Ok {
		return nil, fmt.Errorf("users response not OK: %s", body)
	}

	return users, nil
}

func (c *SlackClient) loadCache() error {
	content, err := os.ReadFile(c.cachePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	return json.Unmarshal(content, &c.cache)
}

func (c *SlackClient) History(channelID string, startTimestamp string, limit int) (*HistoryResponse, error) {
	body, err := c.get("conversations.replies",
		map[string]string{
			"channel":   channelID,
			"ts":        startTimestamp,
			"inclusive": "true"})
	if err != nil {
		return nil, err
	}

	historyResponse := &HistoryResponse{}
	err = json.Unmarshal(body, historyResponse)
	if err != nil {
		return nil, err
	}

	if !historyResponse.Ok {
		return nil, fmt.Errorf("conversations.replies response not OK: %s", body)
	}

	if len(historyResponse.Messages) > 1 {
		// This was a thread, so we can return immediately
		return historyResponse, nil
	}

	// Otherwise we read the general channel history
	body, err = c.get("conversations.history",
		map[string]string{
			"channel":   channelID,
			"oldest":    startTimestamp,
			"inclusive": "true",
			"limit":     strconv.Itoa(limit)})
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(body, historyResponse)
	if err != nil {
		return nil, err
	}

	if !historyResponse.Ok {
		return nil, fmt.Errorf("conversations.history response not OK: %s", body)
	}
	c.log.Println(string(body))
	c.log.Printf("%#v", historyResponse)
	return historyResponse, nil
}

func (c *SlackClient) saveCache() error {
	bs, err := json.Marshal(c.cache)
	if err != nil {
		return err
	}

	err = os.MkdirAll(path.Dir(c.cachePath), 0755)
	if err != nil {
		return err
	}

	err = os.WriteFile(c.cachePath, bs, 0644)
	if err != nil {
		return err
	}

	return nil
}

func (c *SlackClient) getChannelID(name string) (string, error) {
	if id, ok := c.cache.Channels[name]; ok {
		return id, nil
	}

	channels, err := c.conversations(nil)
	if err != nil {
		return "", err
	}

	c.cache.Channels = make(map[string]string)
	for _, ch := range channels {
		c.cache.Channels[ch.Name] = ch.ID
	}

	err = c.saveCache()
	if err != nil {
		return "", err
	}

	if id, ok := c.cache.Channels[name]; ok {
		return id, nil
	}

	return "", fmt.Errorf("no channel with name %q", name)
}

func (c *SlackClient) UsernameForID(id string) (string, error) {
	if id, ok := c.cache.Users[id]; ok {
		return id, nil
	}

	ur, err := c.users(nil)
	if err != nil {
		return "", err
	}

	c.cache.Users = make(map[string]string)
	for _, ch := range ur.Members {
		c.cache.Users[ch.ID] = ch.Name
	}

	err = c.saveCache()
	if err != nil {
		return "", err
	}

	if id, ok := c.cache.Users[id]; ok {
		return id, nil
	}

	return "", fmt.Errorf("no user with id %q", id)
}
