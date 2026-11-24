<template>
  <section class="connector">
    <header class="page-header">
      <h1 class="title is-4 mb-2">
        Claude connector
      </h1>
      <p class="is-size-7 has-text-grey">
        Connect Claude to this listmonk so it can look things up, write draft campaigns and build sequences for you.
      </p>
    </header>

    <div class="columns">
      <div class="column is-8">
        <div class="box">
          <h5 class="title is-6 mb-3">
            1. Add it to Claude
          </h5>
          <p class="mb-3">
            In Claude, open Settings, then Connectors, choose "Add custom connector" and paste this address:
          </p>
          <b-field>
            <b-input :value="info.url" readonly expanded ref="url" data-cy="connector-url" />
            <p class="control">
              <b-button type="is-primary" @click="copyURL" data-cy="btn-copy-connector-url">
                Copy
              </b-button>
            </p>
          </b-field>
          <p class="is-size-7 has-text-grey">
            Claude will open this listmonk and ask you to sign in and approve. It then works as you, with your permissions.
          </p>
        </div>

        <div class="box">
          <h5 class="title is-6 mb-3">
            2. Choose what Claude may do
          </h5>
          <p class="mb-3">
            Claude can always read your lists, subscribers and results, add and update subscribers, and write
            draft campaigns and sequences. It can never delete anything.
          </p>
          <b-field>
            <b-switch v-model="allowSending" :disabled="!$can('settings:manage') || loading.settings"
              @input="saveSending" data-cy="switch-allow-sending">
              Let Claude schedule and send
            </b-switch>
          </b-field>
          <p v-if="allowSending" class="is-size-7 has-text-danger">
            On. Claude can schedule and send campaigns to your lists and switch sequences on, when you ask it to.
          </p>
          <p v-else class="is-size-7 has-text-grey">
            Off. Claude leaves everything as a draft and you schedule or send it here in listmonk.
          </p>
        </div>

        <div class="box">
          <h5 class="title is-6 mb-3">
            Connected
          </h5>
          <p v-if="info.connections.length === 0" class="has-text-grey">
            Nothing is connected yet.
          </p>
          <b-table v-else :data="info.connections" hoverable>
            <b-table-column v-slot="props" field="clientName" label="App">
              {{ props.row.clientName }}
            </b-table-column>
            <b-table-column v-slot="props" field="username" label="Signed in as">
              {{ props.row.username }}
            </b-table-column>
            <b-table-column v-slot="props" field="createdAt" label="Connected">
              {{ $utils.niceDate(props.row.createdAt, true) }}
            </b-table-column>
            <b-table-column v-slot="props" field="lastUsedAt" label="Last used">
              {{ props.row.lastUsedAt ? $utils.niceDate(props.row.lastUsedAt, true) : 'Not yet' }}
            </b-table-column>
            <b-table-column v-slot="props" cell-class="actions" align="right">
              <a v-if="$can('settings:manage')" href="#" @click.prevent="disconnect(props.row)"
                data-cy="btn-disconnect">
                Disconnect
              </a>
            </b-table-column>
          </b-table>
        </div>
      </div>
    </div>
  </section>
</template>

<script>
import Vue from 'vue';
import { mapState } from 'vuex';

export default Vue.extend({
  data() {
    return {
      info: { url: '', allowSending: false, connections: [] },
      allowSending: false,
    };
  },

  methods: {
    setInfo(data) {
      this.info = { ...data, connections: data.connections || [] };
      this.allowSending = data.allowSending;
    },

    getInfo() {
      this.$api.getConnector().then(this.setInfo);
    },

    copyURL() {
      const done = () => this.$utils.toast('Address copied');
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(this.info.url).then(done);
        return;
      }
      this.$refs.url.$el.querySelector('input').select();
      document.execCommand('copy');
      done();
    },

    saveSending(on) {
      const save = () => {
        this.$api.updateConnectorSettings({ allow_sending: on }).then((data) => {
          this.setInfo(data);
          this.$utils.toast(on ? 'Claude can now schedule and send' : 'Claude can no longer schedule or send');
        });
      };

      if (!on) {
        save();
        return;
      }

      this.$utils.confirm(
        'Let Claude schedule and send? It will be able to send campaigns to your lists and switch sequences on when you ask it to.',
        save,
        () => { this.allowSending = false; },
      );
    },

    disconnect(row) {
      this.$utils.confirm(`Disconnect ${row.clientName} (${row.username})? It stops working straight away.`, () => {
        this.$api.deleteConnectorConnection(row.id).then((data) => {
          this.setInfo(data);
          this.$utils.toast('Disconnected');
        });
      });
    },
  },

  computed: {
    ...mapState(['loading']),
  },

  created() {
    this.$root.$on('page.refresh', this.getInfo);
  },

  destroyed() {
    this.$root.$off('page.refresh', this.getInfo);
  },

  mounted() {
    this.getInfo();
  },
});
</script>
