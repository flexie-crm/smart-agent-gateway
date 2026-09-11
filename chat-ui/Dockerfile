# The build context is the REPOSITORY ROOT, not `chat-ui/`, because this front
# end stamps itself with the commit it was built at and the script that does it
# is shared with the console (`scripts/build-stamp.mjs`), one directory above.
FROM node:22-alpine AS build
WORKDIR /src
COPY chat-ui/package*.json ./
RUN npm ci
COPY scripts/ /scripts/
COPY chat-ui/ ./
RUN npm run build

FROM nginx:1.27-alpine
COPY chat-ui/nginx.conf /etc/nginx/conf.d/default.conf
# `dist/app`, not `dist`: this project builds twice from one source (an app and
# an embeddable library, see vite.config.js), so `dist` holds two directories and
# copying it puts index.html one level below where nginx looks. The symptom is
# nginx's own welcome page, which looks like a working deployment.
COPY --from=build /src/dist/app /usr/share/nginx/html
EXPOSE 80
