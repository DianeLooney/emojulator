package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image/png"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/ftrvxmtrx/tga"
	"github.com/getsentry/sentry-go"
	"github.com/nfnt/resize"
	"github.com/pkg/errors"
)

var token string
var sentryURL string
var discord *discordgo.Session

func init() {
	token = os.Getenv("DISCORD_TOKEN")
	sentryURL = os.Getenv("SENTRY_URL")

	var err error
	discord, err = discordgo.New("Bot " + token)
	if err != nil {
		fmt.Println("error creating Discord session,", err)
		return
	}
}

func main() {
	if err := sentry.Init(sentry.ClientOptions{Dsn: sentryURL}); err != nil {
		log.Fatalf("sentry.Init: %s", err)
	}
	defer sentry.Flush(2 * time.Second)

	discord.AddHandler(messageCreate)
	
	if err := discord.Open(); err != nil {
		fmt.Println("error opening connection,", err)
		return
	}
	defer discord.Close()
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	sentryHub := sentry.CurrentHub().Clone()

	var err error
	defer func() {
		dump := func (id *sentry.EventID) {
			if id == nil {
				s.ChannelMessageSend(m.ChannelID, "Unable to generate emoji. ☹️")
			} else {
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Unable to generate emoji. ☹️\nerror_id: %v", *id))
			}
		}

		if err != nil {
			id := sentryHub.CaptureException(err)
			dump(id)
			return
		}
		if r := recover(); r != nil {
			id := sentryHub.Recover(r)
			dump(id)
			return
		}
	}()

	if m.Author.ID == s.State.User.ID {
		return
	}
	if strings.StartsWith(m.Content, "!emoj") {
		return
	}

	sentryHub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetUser(sentry.User{
			ID: m.Author.ID,
		})
	})

	if _, err = s.ChannelMessageSend(m.ChannelID, "Generating..."); err != nil {
		err = errors.Wrap(err, "unable to send message to channel")
		return
	}

	sentryHub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetTag("channel.id", m.ChannelID)
		scope.SetTag("guild.id", m.GuildID)
	})

	g, err := discord.Guild(m.GuildID)
	if err != nil {
		err = errors.Wrap(err, "unable to retrieve guild info")
		return
	}

	buf := new(bytes.Buffer)
	z := zip.NewWriter(buf)
	packName := fmt.Sprintf("TwitchEmotes - %v", g.Name)

	err = filepath.Walk("./DiscordEmotes", func(path string, info os.FileInfo, err error) error {
		if path == "./DiscordEmotes" {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		remappedPath := strings.ReplaceAll(path, "DiscordEmotes", packName)
		w, err := z.Create(remappedPath)
		if err != nil {
			return errors.Wrap(err, "unable to create")
		}
		data, err := ioutil.ReadFile(path)
		if err != nil {
			return errors.Wrap(err, "unable to read source file")
		}
		if strings.Contains(path, "DiscordEmotes.lua") {
			// TODO: Use a template for sappho sake.
			data = bytes.Replace(data, []byte("discord_server_id"), []byte(packName), 1)
			for _, e := range g.Emojis {
				nsEmoji := fmt.Sprintf("discord.%v.%v", g.ID, e.Name)
				data = bytes.Replace(data, []byte("--Pack"), []byte(fmt.Sprintf("['%v']='Interface\\\\AddOns\\\\%v\\\\%v\\\\%v.tga:28:28',\n--Pack", nsEmoji, packName, g.ID, e.Name)), 1)
				data = bytes.Replace(data, []byte("--Emoticons"), []byte(fmt.Sprintf("['%v']='%v',\n--Emoticons", ":"+e.Name+":", nsEmoji)), 1)
			}
		}
		if strings.Contains(path, "DiscordEmotes.toc") {
			data = bytes.Replace(data, []byte("## Title: DiscordEmotes"), []byte(fmt.Sprintf("## Title: %v", packName)), 1)
		}
		_, err = w.Write(data)
		if err != nil {
			err = errors.Wrap(err, "unable to write data to zip")
		}
		return err
	})

	for _, e := range g.Emojis {
		loc := fmt.Sprintf("https://cdn.discordapp.com/emojis/%v.png", e.ID)
		cl := http.Client{}
		d, err := cl.Get(loc)
		if err != nil {
			err = errors.Wrap(err, "unable to download emoji")
			return
		}
		i, err := png.Decode(d.Body)
		if err != nil {
			err = errors.Wrap(err, "unable to decode as png")
			return
		}
		m := resize.Resize(32, 32, i, resize.Lanczos3)
		w, err := z.Create(fmt.Sprintf("%v\\%v\\%v.tga", packName, g.ID, e.Name))
		if err != nil {
			err = errors.Wrap(err, "unable to add file to zip")
			return
		}
		wrt := new(bytes.Buffer)
		err = tga.Encode(wrt, m)
		if err != nil {
			err = errors.Wrap(err, "unable to encode image")
			return
		}
		_, err = w.Write(wrt.Bytes())
		if err != nil {
			err = errors.Wrap(err, "unable to write image data")
			return
		}
	}

	z.Close()

	_, err = s.ChannelFileSendWithMessage(m.ChannelID, "All done! 🎉", fmt.Sprintf("%v.zip", packName), buf)
	if err != nil {
		err = errors.Wrap(err, "unable to send addon to channel")
	}
}
