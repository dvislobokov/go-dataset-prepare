package lib

type Client struct{ name string }

func NewClient(name string) *Client { return &Client{name: name} }

func (c *Client) Name() string { return c.name }
