package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"

	"github.com/docker-library/bashbrew/manifest"
	"github.com/urfave/cli"
)

type dockerfileMetadata struct {
	StageFroms     []string          // every image "FROM" instruction value (or the parent stage's FROM value in the case of a named stage)
	StageNames     []string          // the name of any named stage (in order)
	StageNameFroms map[string]string // map of stage names to FROM values (or the parent stage's FROM value in the case of a named stage), useful for resolving stage names to FROM values

	Froms []string // every "FROM" or "COPY --from=xxx" value (minus named and/or numbered stages in the case of "--from=")
}

// this returns the "FROM" value for the last stage (which essentially determines the "base" for the final published image)
func (r Repo) ArchLastStageFrom(arch string, entry *manifest.Manifest2822Entry) (string, error) {
	dockerfileMeta, err := r.archDockerfileMetadata(arch, entry)
	if err != nil {
		return "", err
	}
	return dockerfileMeta.StageFroms[len(dockerfileMeta.StageFroms)-1], nil
}

func (r Repo) DockerFroms(entry *manifest.Manifest2822Entry) ([]string, error) {
	return r.ArchDockerFroms(arch, entry)
}

func (r Repo) ArchDockerFroms(arch string, entry *manifest.Manifest2822Entry) ([]string, error) {
	dockerfileMeta, err := r.archDockerfileMetadata(arch, entry)
	if err != nil {
		return nil, err
	}
	return dockerfileMeta.Froms, nil
}

func (r Repo) dockerfileMetadata(entry *manifest.Manifest2822Entry) (*dockerfileMetadata, error) {
	return r.archDockerfileMetadata(arch, entry)
}

var dockerfileMetadataCache = map[string]*dockerfileMetadata{}

func (r Repo) archDockerfileMetadata(arch string, entry *manifest.Manifest2822Entry) (*dockerfileMetadata, error) {
	if builder := entry.ArchBuilder(arch); builder == "oci-import" {
		return &dockerfileMetadata{
			Froms: []string{
				"scratch",
			},
		}, nil
	}

	commit, err := r.fetchGitRepo(arch, entry)
	if err != nil {
		return nil, cli.NewMultiError(fmt.Errorf("failed fetching Git repo for arch %q from entry %q", arch, entry.String()), err)
	}

	dockerfileFile := path.Join(entry.ArchDirectory(arch), entry.ArchFile(arch))

	cacheKey := strings.Join([]string{
		commit,
		dockerfileFile,
	}, "\n")
	if meta, ok := dockerfileMetadataCache[cacheKey]; ok {
		return meta, nil
	}

	dockerfile, err := gitShow(commit, dockerfileFile)
	if err != nil {
		return nil, cli.NewMultiError(fmt.Errorf(`failed "git show" for %q from commit %q`, dockerfileFile, commit), err)
	}

	meta, err := parseDockerfileMetadata(dockerfile)
	if err != nil {
		return nil, cli.NewMultiError(fmt.Errorf(`failed parsing Dockerfile metadata for %q from commit %q`, dockerfileFile, commit), err)
	}

	dockerfileMetadataCache[cacheKey] = meta
	return meta, nil
}

func parseDockerfileMetadata(dockerfile string) (*dockerfileMetadata, error) {
	meta := &dockerfileMetadata{
		// panic: assignment to entry in nil map
		StageNameFroms: map[string]string{},
		// (nil slices work fine)
	}

	scanner := bufio.NewScanner(strings.NewReader(dockerfile))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" {
			// ignore blank lines
			continue
		}

		if line[0] == '#' {
			// TODO handle "escape" parser directive
			// TODO handle "syntax" parser directive -- explode appropriately (since custom syntax invalidates our Dockerfile parsing)
			// ignore comments
			continue
		}

		// handle line continuations
		// (TODO see note above regarding "escape" parser directive)
		for line[len(line)-1] == '\\' && scanner.Scan() {
			nextLine := strings.TrimSpace(scanner.Text())
			if nextLine == "" || nextLine[0] == '#' {
				// ignore blank lines and comments
				continue
			}
			line = line[0:len(line)-1] + nextLine
		}

		fields := strings.Fields(line)
		if len(fields) < 1 {
			// must be a much more complex empty line??
			continue
		}
		instruction := strings.ToUpper(fields[0])

		// TODO balk at ARG / $ in from values

		switch instruction {
		case "FROM":
			from := fields[1]

			if stageFrom, ok := meta.StageNameFroms[from]; ok {
				// if this is a valid stage name, we should resolve it back to the original FROM value of that previous stage (we don't care about inter-stage dependencies for the purposes of either tag dependency calculation or tag building -- just how many there are and what external things they require)
				from = stageFrom
			}

			// make sure to add ":latest" if it's implied
			from = latestizeRepoTag(from)

			meta.StageFroms = append(meta.StageFroms, from)
			meta.Froms = append(meta.Froms, from)

			if len(fields) == 4 && strings.ToUpper(fields[2]) == "AS" {
				stageName := fields[3]
				meta.StageNames = append(meta.StageNames, stageName)
				meta.StageNameFroms[stageName] = from
			}
		case "COPY":
			for _, arg := range fields[1:] {
				if !strings.HasPrefix(arg, "--") {
					// doesn't appear to be a "flag"; time to bail!
					break
				}
				if !strings.HasPrefix(arg, "--from=") {
					// ignore any flags we're not interested in
					continue
				}
				from := arg[len("--from="):]

				if stageFrom, ok := meta.StageNameFroms[from]; ok {
					// see note above regarding stage names in FROM
					from = stageFrom
				} else if stageNumber, err := strconv.Atoi(from); err == nil && stageNumber < len(meta.StageFroms) {
					// must be a stage number, we should resolve it too
					from = meta.StageFroms[stageNumber]
				}

				// make sure to add ":latest" if it's implied
				from = latestizeRepoTag(from)

				meta.Froms = append(meta.Froms, from)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return meta, nil
}

func (r Repo) DockerCacheName(entry *manifest.Manifest2822Entry) (string, error) {
	cacheHash, err := r.dockerCacheHash(entry)
	if err != nil {
		return "", err
	}
	return "bashbrew/cache:" + cacheHash, err
}

func (r Repo) dockerCacheHash(entry *manifest.Manifest2822Entry) (string, error) {
	uniqueBits, err := r.dockerBuildUniqueBits(entry)
	if err != nil {
		return "", err
	}
	uniqueString := strings.Join(uniqueBits, "\n")
	b := sha256.Sum256([]byte(uniqueString))
	return hex.EncodeToString(b[:]), nil
}

func dockerInspect(format string, args ...string) (string, error) {
	args = append([]string{"inspect", "-f", format}, args...)
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%v\ncommand: docker inspect -f %q %q\n%s", ee, format, args, string(ee.Stderr))
		}
	}
	return strings.TrimSpace(string(out)), nil
}

var dockerFromIdCache = map[string]string{
	"scratch": "scratch",
}

func (r Repo) dockerBuildUniqueBits(entry *manifest.Manifest2822Entry) ([]string, error) {
	uniqueBits := []string{
		entry.ArchGitRepo(arch),
		entry.ArchGitCommit(arch),
		entry.ArchDirectory(arch),
		entry.ArchFile(arch),
	}
	if builder := entry.ArchBuilder(arch); builder != "" {
		// NOTE: preserve long-term unique id by only attaching builder if
		// explicitly specified
		uniqueBits = append(uniqueBits, entry.ArchBuilder(arch))
	}
	meta, err := r.dockerfileMetadata(entry)
	if err != nil {
		return nil, err
	}
	for _, from := range meta.Froms {
		fromId, ok := dockerFromIdCache[from]
		if !ok {
			var err error
			fromId, err = dockerInspect("{{.Id}}", from)
			if err != nil {
				return nil, err
			}
			dockerFromIdCache[from] = fromId
		}
		uniqueBits = append(uniqueBits, fromId)
	}
	return uniqueBits, nil
}

func dockerBuild(tags []string, file string, context io.Reader, platform string) error {
	args := []string{"build"}
	for _, tag := range tags {
		args = append(args, "--tag", tag)
	}
	if file != "" {
		args = append(args, "--file", file)
	}
	args = append(args, "--rm", "--force-rm", "-")

	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")
	if debugFlag {
		fmt.Println("$ export DOCKER_BUILDKIT=0")
	}
	if platform != "" {
		// ideally, we would set this via an explicit "--platform" flag on "docker build", but it's not supported without buildkit until 20.10+ and this is a trivial way to get Docker to do the right thing in both cases without explicitly trying to detect whether we're on 20.10+
		// https://github.com/docker/cli/blob/v20.10.7/cli/command/image/build.go#L163
		cmd.Env = append(cmd.Env, "DOCKER_DEFAULT_PLATFORM="+platform)
		if debugFlag {
			fmt.Printf("$ export DOCKER_DEFAULT_PLATFORM=%q\n", platform)
		}
	}
	cmd.Stdin = context
	if debugFlag {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		fmt.Printf("$ docker %q\n", args)
		return cmd.Run()
	} else {
		buf := &bytes.Buffer{}
		cmd.Stdout = buf
		cmd.Stderr = buf
		err := cmd.Run()
		if err != nil {
			err = cli.NewMultiError(err, fmt.Errorf(`docker %q output:%s`, args, "\n"+buf.String()))
		}
		return err
	}
}

func dockerBuildFromTarFile(tags []string, file string, tarFile, platform string, froms []string) error {
	tmpDir := strings.TrimSuffix(tarFile, ".tar")
	err := modifyDockerfileFrom(file, tarFile, froms)
	if err != nil {
		return err
	}

	defer func() {
		_, _ = exec.Command("rm", "-rf", tmpDir).Output()
	}()

	args := []string{"build"}
	for _, tag := range tags {
		args = append(args, "--tag", tag)
	}

	args = append(args, "--file", tmpDir+"/"+file)
	args = append(args, "--rm", "--force-rm", tmpDir)

	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")
	if debugFlag {
		fmt.Println("$ export DOCKER_BUILDKIT=0")
	}
	if platform != "" {
		// ideally, we would set this via an explicit "--platform" flag on "docker build", but it's not supported without buildkit until 20.10+ and this is a trivial way to get Docker to do the right thing in both cases without explicitly trying to detect whether we're on 20.10+
		// https://github.com/docker/cli/blob/v20.10.7/cli/command/image/build.go#L163
		cmd.Env = append(cmd.Env, "DOCKER_DEFAULT_PLATFORM="+platform)
		if debugFlag {
			fmt.Printf("$ export DOCKER_DEFAULT_PLATFORM=%q\n", platform)
		}
	}
	if debugFlag {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		fmt.Printf("$ docker %q\n", args)
		return cmd.Run()
	} else {
		buf := &bytes.Buffer{}
		cmd.Stdout = buf
		cmd.Stderr = buf
		err := cmd.Run()
		if err != nil {
			err = cli.NewMultiError(err, fmt.Errorf(`docker %q output:%s`, args, "\n"+buf.String()))
		}
		return err
	}
}

const dockerfileSyntaxEnv = "BASHBREW_BUILDKIT_SYNTAX"

func dockerBuildxBuild(tags []string, file string, tarFile, platform string, froms []string) error {
	tmpDir := strings.TrimSuffix(tarFile, ".tar")
	err := modifyDockerfileFrom(file, tarFile, froms)
	if err != nil {
		return err
	}

	defer func() {
		_, _ = exec.Command("rm", "-rf", tmpDir).Output()
	}()

	dockerfileSyntax, ok := os.LookupEnv(dockerfileSyntaxEnv)
	if !ok {
		return fmt.Errorf("missing %q", dockerfileSyntaxEnv)
	}

	args := []string{
		"buildx",
		"build",
		"--progress", "plain",
		"--build-arg", "BUILDKIT_SYNTAX=" + dockerfileSyntax,
	}
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	for _, tag := range tags {
		args = append(args, "--tag", tag)
	}
	args = append(args, "--file", tmpDir+"/"+file, tmpDir)

	cmd := exec.Command("docker", args...)
	//cmd.Stdin = context
	if debugFlag {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		fmt.Printf("$ docker %q\n", args)
		return cmd.Run()
	} else {
		buf := &bytes.Buffer{}
		cmd.Stdout = buf
		cmd.Stderr = buf
		err := cmd.Run()
		if err != nil {
			err = cli.NewMultiError(err, fmt.Errorf(`docker %q output:%s`, args, "\n"+buf.String()))
		}
		return err
	}
}

func dockerTag(tag1 string, tag2 string) error {
	if debugFlag {
		fmt.Printf("$ docker tag %q %q\n", tag1, tag2)
	}
	_, err := exec.Command("docker", "tag", tag1, tag2).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("%v\ncommand: docker tag %q %q\n%s", ee, tag1, tag2, string(ee.Stderr))
		}
	}
	return err
}

func dockerPush(tag string) error {
	if debugFlag {
		fmt.Printf("$ docker push %q\n", tag)
	}
	_, err := exec.Command("docker", "push", tag).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("%v\ncommand: docker push %q\n%s", ee, tag, string(ee.Stderr))
		}
	}
	return err
}

func dockerPull(tag string) error {
	if debugFlag {
		fmt.Printf("$ docker pull %q\n", tag)
	}

	tag = fmt.Sprintf("%s/library/%s", registryAddress, tag)

	_, err := exec.Command("docker", "pull", tag).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("%v\ncommand: docker pull %q\n%s", ee, tag, string(ee.Stderr))
		}
	}
	return err
}

func modifyDockerfileFrom(file, tarFile string, froms []string) error {
	fromMaps := prepareReplaceFromPair(froms)
	if len(fromMaps) == 0 {
		return nil
	}

	tmpDir := strings.TrimSuffix(tarFile, ".tar")
	fmt.Println(fmt.Sprintf("000000000000 %s", tmpDir))

	tarCmd := []string{"-c", fmt.Sprintf("mkdir -p %s; tar xf %s -C %s", tmpDir, tarFile, tmpDir)}
	_, err := exec.Command("bash", tarCmd...).Output()
	fmt.Println(fmt.Sprintf("1111 err %v", err))
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("%v\ncommand: update tar\n%s", ee, string(ee.Stderr))
		}
	}

	for from, replaced := range fromMaps {
		sedCmd := []string{"-c", fmt.Sprintf(`sed -i "s/FROM %s/FROM %s/g" %s/%s`, from, replaced, tmpDir, file)}
		_, err := exec.Command("bash", sedCmd...).Output()
		fmt.Println(fmt.Sprintf("22222222 err %v", err))
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return fmt.Errorf("%v\ncommand: sed error\n%s", ee, string(ee.Stderr))
			}
		}
	}

	return nil
}

func prepareReplaceFromPair(froms []string) map[string]string {
	fromMaps := make(map[string]string)
	for _, from := range froms {
		if from == "scratch" {
			continue
		}
		if _, exist := fromMaps[from]; exist {
			continue
		}
		fromMaps[from] = fmt.Sprintf(`%s\/library\/%s`, registryAddress, from)
	}
	return fromMaps
}
