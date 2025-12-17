// ftp_client.go
package scripts

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// -----------------------------
// FTP minimal client (PASV)
// -----------------------------
type ftpClientShared struct {
	conn   net.Conn
	reader *bufio.Reader
	user   string
	pass   string
}

func newFtpClientShared(host string, user, pass string) (*ftpClientShared, error) {
	var conn net.Conn
	var err error
	maxRetries := 3
	timeout := 30 * time.Second

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		conn, err = net.DialTimeout("tcp", host+":21", timeout)
		if err == nil {
			break
		}
	}

	if err != nil {
		return nil, fmt.Errorf("falha após %d tentativas: %w", maxRetries, err)
	}

	c := &ftpClientShared{
		conn:   conn,
		reader: bufio.NewReader(conn),
		user:   user,
		pass:   pass,
	}

	if _, err := c.readLine(); err != nil {
		c.conn.Close()
		return nil, err
	}

	if err := c.sendExpect("USER "+user, "331"); err != nil {
		if !strings.HasPrefix(err.Error(), "230") {
			c.conn.Close()
			return nil, fmt.Errorf("USER failed: %w", err)
		}
	}

	if err := c.sendExpect("PASS "+pass, "230"); err != nil {
		if !strings.HasPrefix(err.Error(), "230") {
			c.conn.Close()
			return nil, fmt.Errorf("PASS failed: %w", err)
		}
	}

	return c, nil
}

func (c *ftpClientShared) readLine() (string, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (c *ftpClientShared) writeLine(cmd string) error {
	_, err := c.conn.Write([]byte(cmd + "\r\n"))
	return err
}

func (c *ftpClientShared) sendExpect(cmd, expectPrefix string) error {
	if err := c.writeLine(cmd); err != nil {
		return err
	}
	line, err := c.readLine()
	if err != nil {
		return err
	}
	if expectPrefix != "" && !strings.HasPrefix(line, expectPrefix) {
		return errors.New(line)
	}
	return nil
}

func (c *ftpClientShared) makeDir(path string) error {
	if err := c.writeLine("MKD " + path); err != nil {
		return err
	}
	line, err := c.readLine()
	if err != nil {
		return err
	}
	if strings.HasPrefix(line, "257") || strings.HasPrefix(line, "250") || strings.HasPrefix(line, "550") {
		return nil
	}
	return errors.New(line)
}

func (c *ftpClientShared) size(path string) (int64, error) {
	if err := c.writeLine("SIZE " + path); err != nil {
		return 0, err
	}
	line, err := c.readLine()
	if err != nil {
		return 0, err
	}
	if strings.HasPrefix(line, "213") {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			return n, nil
		}
	}
	if strings.HasPrefix(line, "550") {
		return 0, errors.New("file not found")
	}
	return 0, errors.New(line)
}

func (c *ftpClientShared) enterPassive() (string, int, error) {
	if err := c.writeLine("PASV"); err != nil {
		return "", 0, err
	}
	line, err := c.readLine()
	if err != nil {
		return "", 0, err
	}
	start := strings.Index(line, "(")
	end := strings.Index(line, ")")
	if start < 0 || end < 0 {
		return "", 0, fmt.Errorf("unexpected PASV response: %s", line)
	}
	parts := strings.Split(line[start+1:end], ",")
	if len(parts) < 6 {
		return "", 0, fmt.Errorf("unexpected PASV parts: %v", parts)
	}
	host := parts[0] + "." + parts[1] + "." + parts[2] + "." + parts[3]
	p1, _ := strconv.Atoi(parts[4])
	p2, _ := strconv.Atoi(parts[5])
	port := p1*256 + p2
	return host, port, nil
}

func (c *ftpClientShared) stor(path string, data []byte) error {
	if err := c.writeLine("TYPE I"); err != nil {
		return err
	}
	if _, err := c.readLine(); err != nil {
		return err
	}

	host, port, err := c.enterPassive()
	if err != nil {
		return err
	}

	dataConn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 30*time.Second)
	if err != nil {
		return err
	}
	defer dataConn.Close()

	if err := c.writeLine("STOR " + path); err != nil {
		return err
	}
	if _, err := c.readLine(); err != nil {
		return err
	}

	_, err = dataConn.Write(data)
	if err != nil {
		return err
	}
	dataConn.Close()
	if _, err := c.readLine(); err != nil {
		return err
	}
	return nil
}

func (c *ftpClientShared) quit() {
	_ = c.writeLine("QUIT")
	c.conn.Close()
}

// -----------------------------
// High level helpers (compartilhados)
// -----------------------------

func ftpMakeDirIfNotExists(server, user, pass, path string) error {
	u, err := url.Parse("ftp://" + server + path)
	if err != nil {
		return err
	}
	client, err := newFtpClientShared(u.Host, user, pass)
	if err != nil {
		return err
	}
	defer client.quit()
	return client.makeDir(u.Path)
}

func ftpFileExists(server, user, pass, path string) (bool, error) {
	u, err := url.Parse("ftp://" + server + path)
	if err != nil {
		return false, err
	}
	client, err := newFtpClientShared(u.Host, user, pass)
	if err != nil {
		return false, err
	}
	defer client.quit()
	_, err = client.size(u.Path)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func ftpUploadFile(server, user, pass, remotePath string, data []byte) error {
	u, err := url.Parse("ftp://" + server + remotePath)
	if err != nil {
		return err
	}
	client, err := newFtpClientShared(u.Host, user, pass)
	if err != nil {
		return err
	}
	defer client.quit()
	return client.stor(u.Path, data)
}
