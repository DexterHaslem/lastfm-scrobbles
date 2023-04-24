package main

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const configFile = "sb.json"

type config struct {
	ApiKey        string `json:"apiKey"`
	ApiSecret     string `json:"apiSecret"` // for writes
	UpdateSeconds int    `json:"updateSeconds"`
	ListenAddress string `json:"listenAddress"` // "0.0.0.0:1234"
}

var loadedCfg *config
var forceUpdateChannel = make(chan bool)
var connections = map[string][]*connInfo{}
var updateTs = map[string]int64{}
var wsConns []websocket.Conn
var mutex sync.Mutex

func loadConfig(fileName string) (*config, error) {
	fb, err := os.ReadFile(fileName)
	if err != nil {
		return nil, err
	}
	newCfg := &config{}
	err = json.Unmarshal(fb, &newCfg)
	return newCfg, err
}

type connInfo struct {
	conn       *websocket.Conn
	userName   string
	remoteAddr string
	first      bool
}

// TODO:
//type cachedTrackInfo struct {
//	tracks *[]TrackInfo
//	lastTs int64
//}

//var trackCache = map[string][]*cachedTrackInfo{}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin == "" || strings.HasPrefix(origin, "http://localhost") ||
			strings.HasPrefix(origin, "http://127.0.0.1") ||
			origin == "https://api.dmh.fm:8881" ||
			origin == "https://dexter.ahacheers.com"
	},
}

type ImgInfo struct {
	Size string `json:"size"`
	Text string `json:"#text"`
}

type LastFmTrack struct {
	Artist *struct {
		Name string `json:"#text,omitempty"` // beware firefox shows as 'name'
	} `json:"artist,omitempty"`

	Album *struct {
		Name string `json:"#text"`
	} `json:"album,omitempty"`

	Name  string     `json:"name,omitempty"`
	Image []*ImgInfo `json:"image,omitempty"`

	Attr *struct {
		NowPlaying string `json:"nowplaying,omitempty"`
	} `json:"@attr,omitempty"`

	Date *struct {
		Uts string `json:"uts,omitempty"`
	} `json:"date,omitempty"`

	Url string `json:"url,omitempty"`
}

type TrackInfo struct {
	Title        string `json:"title,omitempty"`
	Album        string `json:"album,omitempty"`
	IsNowPlaying bool   `json:"nowplaying,omitempty"`
	Artist       string `json:"artist,omitempty"`
	ImgSrc       string `json:"img,omitempty"`
	Url          string `json:"url,omitempty"`
	Time         string `json:"time,omitempty"`
	Hash         string `json:"hash"`
}

func (ti *TrackInfo) CalcHash() {
	h := md5.New()
	_, _ = io.WriteString(h, ti.Url)
	// these can be empty
	_, _ = io.WriteString(h, ti.Artist)
	_, _ = io.WriteString(h, ti.Album)
	_, _ = io.WriteString(h, ti.Title)
	//_, _ = io.WriteString(h, fmt.Sprintf("%v", ti.IsNowPlaying))
	//_, _ = io.WriteString(h, fmt.Sprintf("%v", ti.Time))
	sum := h.Sum(nil)
	ti.Hash = fmt.Sprintf("%x", sum)
}

func findBiggestImgSrc(infos []*ImgInfo) string {
	var found = infos[0]
	for _, ii := range infos {
		if ii.Size == "extralarge" {
			found = ii
		}
	}

	return found.Text
}

func getRecentTracks(url string) (error, []*TrackInfo) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Printf("client: could not create request: %s\n", err)
		os.Exit(1)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err, nil
	}

	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		return err, nil
	}

	// dont unmarshal directly into LastfmRecentTrackResponse
	// track can be an array or object.. ugh so dumb
	tmp := struct {
		RecentTracks struct {
			Track *json.RawMessage
			Attr  struct {
				User       string
				TotalPages string
				Total      string
				Page       string
				PerPage    string
			} `json:"@attr"`
		}
	}{}
	err = json.Unmarshal(resBody, &tmp)
	if err != nil {
		return err, nil
	}

	// lame: track can be an array or object because lastfm thought this was a good idea
	// only ballpark way to tell is checking total, its 0 when only now playing is returned
	// erm note: cant use "total" "0". sometimes it returns an empty response while streaming! wtf
	// just try array first
	var lfmTracks []*LastFmTrack
	rb, _ := tmp.RecentTracks.Track.MarshalJSON()
	err = json.Unmarshal(rb, &lfmTracks)
	if err != nil {
		// most recent track is not array
		lfmTrack := &LastFmTrack{}
		err = json.Unmarshal(rb, &lfmTrack)
		lfmTracks = []*LastFmTrack{lfmTrack}
	}

	if err != nil {
		return err, nil
	}

	var ret []*TrackInfo
	for _, t := range lfmTracks {
		newTi := &TrackInfo{
			Title:        t.Name,
			Album:        t.Album.Name,
			IsNowPlaying: t.Attr != nil && t.Attr.NowPlaying == "true",
			Artist:       t.Artist.Name,
			ImgSrc:       findBiggestImgSrc(t.Image),
			Url:          t.Url,
		}
		if t.Date != nil {
			newTi.Time = t.Date.Uts
		}

		newTi.CalcHash()
		ret = append(ret, newTi)
	}

	return nil, ret
}

func getSince(username string, since int64) (error, []*TrackInfo) {
	timeUnix := strconv.FormatInt(since, 10)
	requestURL := fmt.Sprintf("https://ws.audioscrobbler.com/2.0/?method=user.getrecenttracks&user=%s&api_key=%s&format=json&nowplaying=true&from=%s",
		username, loadedCfg.ApiKey, timeUnix)
	return getRecentTracks(requestURL)
}

func endWs(info *connInfo) {

}

func pingClients() {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mutex.Lock()

	toRemove := map[string][]int{}
	for _, v := range connections {
		for idx, ci := range v {
			pingErr := ci.conn.WriteMessage(websocket.PingMessage, []byte(ts))
			if pingErr != nil {
				log.Printf("ping error %s\n", ci.remoteAddr)
				log.Println(pingErr)

				v, ok := toRemove[ci.userName]
				if !ok {
					v = []int{}
					toRemove[ci.userName] = v
				}
				toRemove[ci.userName] = append(toRemove[ci.userName], idx)
			}
		}
	}
	mutex.Unlock()
	removeConnections(toRemove)

}

func streamingWorker() { //remoteAddr, userName string, ws *websocket.Conn) {
	ticker := time.NewTicker(time.Millisecond * 25)
	pinger := time.NewTicker(time.Second * 3)

	initialDone := false
	defer func() {
		pinger.Stop()
		ticker.Stop()
	}()

	for {
		select {
		case <-forceUpdateChannel:
			log.Println("got force update channel")
			updateAllUserClients()
		case <-pinger.C:
			pingClients()
		case <-ticker.C:
			if !initialDone {
				ticker.Stop()
				ticker = time.NewTicker(time.Second * time.Duration(loadedCfg.UpdateSeconds))
				initialDone = true
			}
			updateAllUserClients()
		}
	}
}

func updateAllUserClients() {
	mutex.Lock()
	copyMap := connections
	mutex.Unlock()

	for userName, conns := range copyMap {
		err, newTracks := getSince(userName, updateTs[userName])
		if err != nil {
			log.Println(err)
			return
		}

		// TODO: caching
		//
		//v, ok := trackCache[userName]
		//if ok {
		//	trackCache[userName] = newTracks
		//} else {
		//	trackCache[userName] = newTracks
		//}
		updateTs[userName] = time.Now().Unix()
		toRemove := map[string][]int{}

		for idx, ci := range conns {
			err = ci.conn.WriteJSON(&newTracks)
			if err != nil {
				log.Printf("got error writing to websocket for %s\n", ci.remoteAddr)
				log.Println(err)
				v, ok := toRemove[ci.userName]
				if !ok {
					v = []int{}
					toRemove[ci.userName] = v
				}
				toRemove[ci.userName] = append(toRemove[ci.userName], idx)
			}
		}

		removeConnections(toRemove)
	}
}

func removeConnections(toRemove map[string][]int) {
	mutex.Lock()
	for k, indices := range toRemove {
		orig := connections[k]
		connections[k] = []*connInfo{}

		for i, v := range orig {
			removed := false
			for _, removeIdxV := range indices {
				if i == removeIdxV {
					removed = true
				}
			}
			if !removed {
				connections[k] = append(connections[k], v)
			} else {
				fmt.Printf("removed connection %s:%s\r\n", v.userName, v.remoteAddr)
			}
		}
	}
	mutex.Unlock()
	//log.Println("exit rmove connections")
}

func streaming(w http.ResponseWriter, r *http.Request) {
	remoteAddr := r.Header.Get("X-Forwarded-For")
	log.Printf("got connection on /streaming from %s\n", remoteAddr)

	q := r.URL.Query()
	userName := q.Get("username")
	if userName == "" {
		http.Error(w, "username required", 401)
		log.Printf("no username\r\n")
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)

	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}

	userName = strings.Trim(userName, " \r\n")

	newConn := &connInfo{
		conn:       c,
		userName:   userName,
		remoteAddr: remoteAddr,
		first:      true,
	}

	mutex.Lock()
	v, ok := connections[userName]
	updateTs[userName] = 0
	if ok {
		connections[userName] = append(v, newConn)
	} else {
		connections[userName] = []*connInfo{newConn}
	}

	newConn.conn.SetCloseHandler(func(code int, text string) error {
		log.Printf("got ws close for %s:%s\n", newConn.userName, newConn.remoteAddr)
		return nil
	})
	mutex.Unlock()
	forceUpdateChannel <- true
}

func main() {
	//flag.Parse()
	tryCfg, err := loadConfig(configFile)
	if err != nil {
		log.Fatalf("Failed to load config file %s: %s\n", configFile, err)
		os.Exit(1)
	}
	loadedCfg = &config{}
	*loadedCfg = *tryCfg

	log.SetFlags(log.Ldate | log.Lmsgprefix)
	go streamingWorker()
	http.HandleFunc("/streaming", streaming)
	log.Fatal(http.ListenAndServe(loadedCfg.ListenAddress, nil))
}
