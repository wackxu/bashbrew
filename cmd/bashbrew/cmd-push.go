package main

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/urfave/cli"
)

func cmdPush(c *cli.Context) error {
	repos, err := repos(c.Bool("all"), c.Args()...)
	if err != nil {
		return cli.NewMultiError(fmt.Errorf(`failed gathering repo list`), err)
	}

	uniq := c.Bool("uniq")
	targetNamespace := c.String("target-namespace")
	dryRun := c.Bool("dry-run")
	force := c.Bool("force")

	if targetNamespace == "" {
		targetNamespace = namespace
	}
	if targetNamespace == "" {
		return fmt.Errorf(`either "--target-namespace" or "--namespace" is a required flag for "push"`)
	}

	for _, repo := range repos {
		r, err := fetch(repo)
		if err != nil {
			return cli.NewMultiError(fmt.Errorf(`failed fetching repo %q`, repo), err)
		}

		tagRepo := path.Join(registryAddress, "library", targetNamespace, r.RepoName)
		for _, entry := range r.Entries() {
			if r.SkipConstraints(entry) {
				continue
			}

			tags := []string{}
			// we can't use "r.Tags()" here because it will include SharedTags, which we never want to push directly (see "cmd-put-shared.go")
			for i, tag := range entry.Tags {
				if uniq && i > 0 {
					break
				}
				tag = tagRepo + ":" + tag
				tags = append(tags, tag)
			}

			switch builder := entry.ArchBuilder(arch); builder {
			case "oci-import":
				cacheTag, err := r.DockerCacheName(entry)
				if err != nil {
					return cli.NewMultiError(fmt.Errorf(`failed calculating "cache hash" for %q (tags %q)`, r.RepoName, entry.TagsString()), err)
				}
				desc, err := ociImportLookup(cacheTag)
				if err != nil {
					return cli.NewMultiError(fmt.Errorf(`failed looking up descriptor for %q (tags %q)`, r.RepoName, entry.TagsString()), err)
				}
				skip, update, err := ociImportPushFilter(*desc, tags)
				if err != nil {
					return cli.NewMultiError(fmt.Errorf(`failed looking up tags for %q (tags %q)`, r.RepoName, entry.TagsString()), err)
				}
				if len(skip) > 0 && len(update) == 0 {
					fmt.Fprintf(os.Stderr, "skipping %s (remote tags all up-to-date)\n", r.EntryIdentifier(entry))
					continue
				} else if len(skip) > 0 {
					fmt.Fprintf(os.Stderr, "partially skipping %s (remote tags up-to-date: %s)\n", r.EntryIdentifier(entry), strings.Join(skip, ", "))
				}
				fmt.Printf("Pushing %s to %s\n", desc.Digest, strings.Join(update, ", "))
				if !dryRun {
					err := ociImportPush(*desc, update)
					if err != nil {
						return cli.NewMultiError(fmt.Errorf(`failed pushing %q`, r.EntryIdentifier(entry)), err)
					}
				}

			default:
			TagsLoop:
				for _, tag := range tags {
					if !force {
						localImageId, _ := dockerInspect("{{.Id}}", tag)
						if debugFlag {
							fmt.Printf("DEBUG: docker inspect %q -> %q\n", tag, localImageId)
						}
						registryImageIds := fetchRegistryImageIds(tag)
						if debugFlag {
							fmt.Printf("DEBUG: registry inspect %q -> %+v\n", tag, registryImageIds)
						}
						for _, registryImageId := range registryImageIds {
							if localImageId == registryImageId {
								fmt.Fprintf(os.Stderr, "skipping %s (remote image matches local)\n", tag)
								continue TagsLoop
							}
						}
					}
					fmt.Printf("Pushing %s\n", tag)
					if !dryRun {
						err = dockerPush(tag)
						if err != nil {
							return cli.NewMultiError(fmt.Errorf(`failed pushing %q`, tag), err)
						}
					}
				}
			}
		}
	}

	return nil
}
