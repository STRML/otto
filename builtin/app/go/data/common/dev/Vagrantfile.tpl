# Generated by Otto, do not edit!
#
# This is the Vagrantfile generated by Otto for the development of
# this application/service. It should not be hand-edited. To modify the
# Vagrantfile, use the Appfile.

Vagrant.configure("2") do |config|
  config.vm.box = "hashicorp/precise64"

  # Setup a synced folder from our working directory to /vagrant
  config.vm.synced_folder "{{ path.working }}", "/vagrant"

  # Make it so that `vagrant ssh` goes directly to the correct dir
  config.vm.provision "shell", inline:
    %Q[echo "cd /vagrant" >> /home/vagrant/.bashrc]
end